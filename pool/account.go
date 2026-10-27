// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-go/config"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// Error-cooldown tuning. When an account hits errorCooldownThreshold consecutive
// errors it is cooled down with exponential backoff (base, 2×base, 4×base …) capped
// at errorCooldownMax, plus ±errorCooldownJitterFraction jitter so co-failing
// accounts recover at staggered times instead of stampeding upstream together.
const (
	errorCooldownBase           = 1 * time.Minute
	errorCooldownMax            = 8 * time.Minute
	errorCooldownThreshold      = 3
	errorCooldownJitterFraction = 0.1
)

// errorCooldownDuration returns the cooldown for a given consecutive-error count:
// exponential from errorCooldownBase (starting at the threshold) up to
// errorCooldownMax, with ±10% jitter to desynchronize simultaneous recoveries.
func errorCooldownDuration(consecutiveErrors int) time.Duration {
	steps := consecutiveErrors - errorCooldownThreshold
	if steps < 0 {
		steps = 0
	}
	// Bound the shift to prevent overflow and to match the cap semantics.
	if steps > 16 {
		steps = 16
	}
	backoff := errorCooldownBase << uint(steps)
	if backoff > errorCooldownMax || backoff <= 0 {
		backoff = errorCooldownMax
	}
	jitter := time.Duration(float64(backoff) * errorCooldownJitterFraction * (2*rand.Float64() - 1))
	d := backoff + jitter
	if d <= 0 {
		d = backoff
	}
	return d
}

// AccountPool 账号池
type AccountPool struct {
	mu            sync.RWMutex
	accounts      []config.Account
	totalAccounts int
	currentIndex  uint64
	cooldowns     map[string]time.Time       // 账号冷却时间（瞬时错误退避）
	errorCounts   map[string]int             // 连续错误计数
	modelLists    map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	runtimeStats  map[string]*accountRuntimeStats // accountID → dispatch health/load signals (health-scoring)
	// quotaCooldowns 单独存放"配额耗尽"冷却。不能和 cooldowns 混用：同一账号上
	// 并发的其他请求成功后会调用 RecordSuccess 清空 cooldowns，那样会把刚因配额
	// 耗尽而拉黑的账号立刻放回轮询，导致它反复被选中、反复失败。
	quotaCooldowns map[string]time.Time
	// localUsage 记录自上次上游用量刷新以来本地累计消耗的 credits。
	// 上游 UsageCurrent 最多有 30 分钟延迟，调度用它做实时配额估算，
	// 从而在账号真正耗尽前就把流量挪走（不参与 isQuotaBlocked 判定，
	// 避免估算误差导致账号被错误地完全排除）。
	localUsage map[string]float64
	// swing 是平滑加权轮询（SWRR）的当前权值，按账号 ID 累计。
	swing map[string]float64
}

// 全局变量声明后初始化（保持 zero value 无副作用）
var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:      make(map[string]time.Time),
			quotaCooldowns: make(map[string]time.Time),
			errorCounts:    make(map[string]int),
			modelLists:     make(map[string]map[string]bool),
			runtimeStats:   make(map[string]*accountRuntimeStats),
			localUsage:     make(map[string]float64),
			swing:          make(map[string]float64),
		}
		pool.Reload()
	})
	return pool
}

// Reload rebuilds the routable account list from config.
//
// The list is deduplicated: the operator-configured priority weight is applied
// by the weighted scheduler (candidateDispatchWeight) rather than by cloning the
// account N times, so GetAllAccounts/Count and the background workers see each
// account exactly once.
//
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var ready []config.Account
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		ready = append(ready, a)
	}
	p.accounts = ready
	// Count() keeps counting every enabled account (quota-blocked ones
	// included) so the admin panel's account total is unchanged.
	p.totalAccounts = len(enabled)
	// Account membership just changed: drop the SWRR accumulators (so a
	// newcomer cannot inherit swing credit). The local usage estimate is kept
	// per account — it is cleared by ResetLocalUsage when that account's
	// upstream usage is refreshed, so concurrent requests are not lost.
	p.swing = make(map[string]float64)
	inPool := make(map[string]bool, len(ready))
	for _, a := range ready {
		inPool[a.ID] = true
	}
	for id := range p.localUsage {
		if !inPool[id] {
			delete(p.localUsage, id)
		}
	}
	// 顺手清掉过期的配额冷却，避免这个 map 无限增长。
	now := time.Now()
	for id, until := range p.quotaCooldowns {
		if !now.Before(until) {
			delete(p.quotaCooldowns, id)
		}
	}
}

// ResetLocalUsage drops the locally accumulated consumption estimate for one
// account. Call it immediately after that account's upstream usage has been
// refreshed: the verified UsageCurrent already includes everything counted
// locally, so keeping the delta would double-count it.
func (p *AccountPool) ResetLocalUsage(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.localUsage != nil {
		delete(p.localUsage, id)
	}
}

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（加权轮询），并跳过指定账号。
//
// Selection is delegated to selectAccountLocked (pool/dispatch.go), which among
// the accounts passing the hard filters (not cooling down, token valid, quota
// available) picks the healthiest/least-loaded one via the smart scheduler and
// stamps it as in-flight. The least-cooldown fallback is preserved inside
// selectAccountLocked so availability behaviour is unchanged.
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selectAccountLocked("", excluded, false)
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding 获取下一个支持指定模型的可用账号，并跳过指定账号。
// Delegates to selectAccountLocked (requireModel=true). See GetNextExcluding.
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selectAccountLocked(model, excluded, true)
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return copyAccount(&p.accounts[i])
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除瞬时错误冷却。
//
// 注意：这里只清 cooldowns，不清 quotaCooldowns —— 同一账号上并发的其他请求
// 成功，并不能证明配额没有被耗尽。若把配额冷却一起清掉，已耗尽的账号会立刻
// 重新进入轮询并反复失败（每次失败又会给它自己加冷却），既浪费请求也造成中断。
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensurePoolMapsLocked()
	p.finishAccountUseLocked(id, true, false, time.Now())
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ensurePoolMapsLocked()
	p.finishAccountUseLocked(id, false, isQuotaError, time.Now())
	if isQuotaError {
		// 配额错误：单独记入 quotaCooldowns，冷却 1 小时。
		// 不进 errorCounts —— 配额耗尽是确定性状态，不是"连续失败"，因此也不该
		// 被后续任意一次成功请求清零。
		p.quotaCooldowns[id] = time.Now().Add(time.Hour)
		return
	}
	p.errorCounts[id]++
	if p.errorCounts[id] >= errorCooldownThreshold {
		// 连续错误达阈值：指数退避（1m→2m→4m→…封顶 8m）+ ±10% 抖动。
		// 越错越久给上游喘息；抖动错开各账号解禁时刻，避免被罚账号一窝蜂同时
		// 涌回再一起撞墙（thundering herd）。
		p.cooldowns[id] = time.Now().Add(errorCooldownDuration(p.errorCounts[id]))
	}
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	p.ensurePoolMapsLocked()
	// Release any in-flight slot the account held before evicting it, so the
	// smart scheduler's in-flight counts stay accurate across disable/re-enable.
	p.finishAccountUseLocked(id, false, false, time.Now())
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
//
// The cooldown goes into quotaCooldowns (not cooldowns) so a concurrent
// successful request on the same account cannot lift it.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.ensurePoolMapsLocked()
	p.finishAccountUseLocked(id, false, false, time.Now())
	p.quotaCooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if _, cooling := p.effectiveCooldownLocked(acc.ID, now); cooling {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensurePoolMapsLocked()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()
				// 实时配额估算：上游 UsageCurrent 最多 30 分钟才刷新一次，
				// 这里把本次请求真实消耗的 credits（meteringEvent）累加到本地
				// 用量上，使调度器立刻感知"哪个账号快用完了"，而不是等到
				// 上游返回 402 之后才把流量挪走。
				if credits > 0 {
					p.localUsage[id] += credits
				}

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

// copyAccount returns a shallow copy of the account, safe to use after
// the pool lock is released.
func copyAccount(acc *config.Account) *config.Account {
	c := *acc
	return &c
}
