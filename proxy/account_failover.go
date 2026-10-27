package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// absoluteMaxAccountRetryAttempts is a defensive ceiling on per-request account
// retries. The budget otherwise means "iterate every selectable account" (see
// resolveAccountRetryBudget): each retry excludes already-tried accounts and
// must pick a different one, so the real retry count converges to the number of
// accounts and never runs away. This only guards pathological cases.
const absoluteMaxAccountRetryAttempts = 64

// resolveAccountRetryBudget returns how many account attempts one request may
// make. Previously this was a fixed 3, so with >3 accounts a request only tried
// the first 3 and gave up while the rest sat idle — under a 429 storm that meant
// an outright failure. Now it scales with the account count so every account
// can be tried. totalAccounts<=0 falls back to 1 (try at least once).
func resolveAccountRetryBudget(totalAccounts int) int {
	if totalAccounts <= 0 {
		return 1
	}
	if totalAccounts > absoluteMaxAccountRetryAttempts {
		return absoluteMaxAccountRetryAttempts
	}
	return totalAccounts
}

// accountRetryBackoff returns the wait before the next retry attempt:
// exponential 200ms→2s cap + jitter, so a transient upstream blip isn't
// amplified by back-to-back retries against the next account. attempt is 0-based.
func accountRetryBackoff(attempt int) time.Duration {
	const baseMS = 200
	const maxMS = 2000
	if attempt < 0 {
		attempt = 0
	}
	shift := attempt
	if shift > 4 {
		shift = 4
	}
	backoff := baseMS << shift
	if backoff > maxMS {
		backoff = maxMS
	}
	jitter := 0
	if j := backoff / 4; j > 0 {
		jitter = rand.Intn(j)
	}
	return time.Duration(backoff+jitter) * time.Millisecond
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

// isQuotaExhaustedMessage narrows isQuotaErrorMessage down to "the quota is
// actually used up". A plain 429 is a rate limit and must NEVER disable an
// account: it recovers on its own within seconds, and disabling on it would take
// healthy capacity out of the pool permanently.
func isQuotaExhaustedMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, marker := range []string{
		"quota exhausted",
		"quota exceeded",
		"quota limit",
		"insufficient quota",
		"no quota",
		"overage",
		"quota_exhausted",
		"quota_exceeded",
		"quota_limit",
	} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	// HTTP 402 Payment Required: the upstream billing/quota limit was reached.
	if strings.Contains(m, "402") && strings.Contains(m, "limit") {
		return true
	}
	return false
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()
	// A ban/suspension is not a quota state: drop the quota marker so the quota
	// recovery sweep can never silently re-enable a banned account.
	updatedAccount.AutoDisabledReason = ""

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	// D3: notify webhook (account id/email + ban status/reason only — no tokens).
	notifyWebhook("account.banned", map[string]interface{}{
		"accountId": account.ID,
		"email":     account.Email,
		"banStatus": banStatus,
		"banReason": banReason,
	})
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isUpstreamPermanentError(err):
		// The request itself is malformed/rejected by the upstream regardless of
		// which account relays it. Penalising the account's health, cooling it
		// down, or rotating to another account would be wrong (the account is
		// healthy) and harmful (scatters cache affinity, wastes upstream hits).
		// Neutral: release the in-flight slot the selection took (RecordPermanentRejection)
		// without recording any success or failure, then return.
		h.pool.RecordPermanentRejection(account.ID)
		return
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, true)
		// 402 OVERAGE：额度确实已经用尽。直接禁用，避免它继续被轮询到而反复报错。
		h.disableQuotaExhaustedAccount(account, "overage limit reached")
	case isQuotaErrorMessage(errMsg):
		if isQuotaExhaustedMessage(errMsg) {
			// 明确的额度耗尽：禁用账号（并从轮询里摘掉）。
			h.pool.RecordError(account.ID, true)
			h.disableQuotaExhaustedAccount(account, "")
		} else {
			// 普通 429 限流：只是短时拥塞，按瞬时错误做指数退避即可。
			// 绝不能当成"额度耗尽"处理，否则健康账号会被长时间踢出池子。
			h.pool.RecordError(account.ID, false)
		}
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}

// ---------------------------------------------------------------------------
// Quota auto-disable / auto-recover
//
// An account whose quota is used up cannot serve any request: every dispatch to
// it fails upstream (402 / 429), and during the window before the pool notices
// it the request error surfaces to the client as an interruption. Cooling it
// down is not enough — it keeps coming back and keeps failing. So the account is
// disabled outright and removed from routing, with a marker that lets the proxy
// re-enable it by itself once the quota comes back (monthly reset / raised
// limit), instead of waiting for an operator.
//
// Operators who opted into over-usage (global AllowOverUsage, or the per-account
// upstream Overages switch) explicitly want exhausted accounts to keep serving,
// so auto-disable is skipped for them.
// ---------------------------------------------------------------------------

// quotaAutoDisableAllowed reports whether the proxy may disable this account for
// quota exhaustion.
func quotaAutoDisableAllowed(account *config.Account) bool {
	if account == nil {
		return false
	}
	if config.GetAllowOverUsage() {
		return false // global "keep serving over quota" switch
	}
	if strings.EqualFold(account.OverageStatus, "ENABLED") {
		return false // per-account upstream Overages switch
	}
	return true
}

// disableQuotaExhaustedAccount disables an account whose usage quota is used up
// and records a readable reason. Idempotent: repeated detections are a no-op.
func (h *Handler) disableQuotaExhaustedAccount(account *config.Account, detail string) {
	if account == nil || !quotaAutoDisableAllowed(account) {
		return
	}
	reason := "quota exhausted"
	if detail != "" {
		reason += " (" + detail + ")"
	}
	changed, err := config.SetAccountAutoDisabledForQuota(account.ID, reason)
	if err != nil {
		logger.Warnf("[AccountFailover] Failed to auto-disable %s for quota: %v", account.Email, err)
		return
	}
	if !changed {
		return
	}
	logger.Warnf("[AccountFailover] Auto-disabled %s: %s", account.Email, reason)
	notifyWebhook("account.quota_exhausted", map[string]interface{}{
		"accountId": account.ID,
		"email":     account.Email,
		"banStatus": "DISABLED",
		"banReason": reason,
	})
	h.pool.Reload()
}

// reenableQuotaRecoveredAccount re-enables an account the proxy had disabled for
// quota exhaustion, once its usage is back under the limit. Returns true when the
// account transitioned back to enabled.
func (h *Handler) reenableQuotaRecoveredAccount(account *config.Account) bool {
	if account == nil || !account.IsAutoQuotaDisabled() {
		return false
	}
	ok, err := config.ReenableAutoQuotaDisabled(account.ID)
	if err != nil {
		logger.Warnf("[AccountFailover] Failed to re-enable %s: %v", account.Email, err)
		return false
	}
	if !ok {
		return false
	}
	logger.Infof("[AccountFailover] Re-enabled %s: quota recovered", account.Email)
	notifyWebhook("account.quota_recovered", map[string]interface{}{
		"accountId": account.ID,
		"email":     account.Email,
	})
	h.pool.Reload()
	return true
}

// applyQuotaStateFromInfo reconciles an account's enabled state with freshly
// fetched usage limits: disable when exhausted, re-enable a quota-disabled
// account once usage falls back under the limit.
func (h *Handler) applyQuotaStateFromInfo(account *config.Account, info *config.AccountInfo) {
	if account == nil || info == nil || info.UsageLimit <= 0 {
		return
	}
	if info.UsageCurrent >= info.UsageLimit {
		if account.Enabled {
			h.disableQuotaExhaustedAccount(account, formatQuotaUsage(info.UsageCurrent, info.UsageLimit))
		}
		return
	}
	// Usage is back under the limit (e.g. the monthly reset landed).
	h.reenableQuotaRecoveredAccount(account)
}

// tryRecoverQuotaDisabledAccount refreshes a quota-disabled account's usage and
// re-enables it when the quota came back. Used by the periodic health check so
// recovery does not wait for the 30-minute background refresh. Returns true when
// the account was re-enabled.
func (h *Handler) tryRecoverQuotaDisabledAccount(account *config.Account) bool {
	if account == nil || !account.IsAutoQuotaDisabled() {
		return false
	}
	info, err := RefreshAccountInfo(account)
	if err != nil {
		logger.Warnf("[AccountFailover] Quota-recovery refresh failed for %s: %v", account.Email, err)
		return false
	}
	if err := config.UpdateAccountInfo(account.ID, *info); err != nil {
		logger.Warnf("[AccountFailover] Quota-recovery persist failed for %s: %v", account.Email, err)
		return false
	}
	if info.UsageLimit <= 0 || info.UsageCurrent >= info.UsageLimit {
		return false
	}
	return h.reenableQuotaRecoveredAccount(account)
}

// formatQuotaUsage renders "12.3/50.0" for the disable reason string.
func formatQuotaUsage(current, limit float64) string {
	return strconv.FormatFloat(current, 'f', 1, 64) + "/" + strconv.FormatFloat(limit, 'f', 1, 64)
}
