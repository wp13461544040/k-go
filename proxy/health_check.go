package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"sync"
	"time"
)

const healthCheckInterval = 5 * time.Minute

// AccountHealthResult 单个账号的健康检查结果
type AccountHealthResult struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	Status    string `json:"status"` // "ok" | "refreshed" | "error" | "skipped"
	Error     string `json:"error,omitempty"`
}

// HealthCheckState 健康检查状态（线程安全）
type HealthCheckState struct {
	mu           sync.RWMutex
	LastRunAt    int64
	NextRunAt    int64
	Running      bool
	TotalChecked int
	Healthy      int
	Unhealthy    int
	Skipped      int
	Results      []AccountHealthResult
}

func newHealthCheckState() *HealthCheckState {
	return &HealthCheckState{}
}

// backgroundHealthCheck 每 5 分钟轻量检查所有账号 Token 有效性
func (h *Handler) backgroundHealthCheck() {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	// 延迟 45 秒，让 backgroundRefresh / backgroundWarmup 先完成初次运行
	time.Sleep(45 * time.Second)
	h.runHealthCheck()

	for {
		select {
		case <-ticker.C:
			h.runHealthCheck()
		case <-h.stopRefresh:
			return
		}
	}
}

// runHealthCheck 执行一次健康检查（并发安全，正在运行时跳过重复调用）
func (h *Handler) runHealthCheck() {
	hc := h.hc

	hc.mu.Lock()
	if hc.Running {
		hc.mu.Unlock()
		return
	}
	hc.Running = true
	hc.mu.Unlock()

	defer func() {
		hc.mu.Lock()
		hc.Running = false
		hc.NextRunAt = time.Now().Add(healthCheckInterval).Unix()
		hc.mu.Unlock()
	}()

	logger.Infof("[HealthCheck] 开始账号健康检查")
	accounts := config.GetAccounts()
	now := time.Now().Unix()

	var results []AccountHealthResult
	healthy, unhealthy, skipped := 0, 0, 0

	for i := range accounts {
		account := &accounts[i]
		result := AccountHealthResult{
			AccountID: account.ID,
			Email:     account.Email,
		}

		if !account.Enabled {
			// 因配额耗尽被自动禁用的账号：顺带探测额度是否已恢复，恢复则自动启用。
			if account.IsAutoQuotaDisabled() && h.tryRecoverQuotaDisabledAccount(account) {
				result.Status = "ok"
				healthy++
			} else {
				result.Status = "skipped"
				skipped++
			}
			results = append(results, result)
			continue
		}

		// 封禁/暂停账号：直接判定异常（这是请求中断的常见原因）
		if banStatus := strings.ToUpper(account.BanStatus); banStatus == "BANNED" || banStatus == "SUSPENDED" {
			result.Status = "error"
			result.Error = banStatus
			if account.BanReason != "" {
				result.Error += ": " + account.BanReason
			}
			unhealthy++
			results = append(results, result)
			continue
		}

		// API Key 账号无需刷新 token
		if account.AuthMethod == "api_key" {
			result.Status = "ok"
			healthy++
			results = append(results, result)
			continue
		}

		// 没有 RefreshToken 的账号跳过（如仅有 AccessToken 的静态账号）
		if account.RefreshToken == "" {
			result.Status = "skipped"
			skipped++
			results = append(results, result)
			continue
		}

		// Token 已过期：强制刷新
		if account.ExpiresAt > 0 && now >= account.ExpiresAt {
			if err := h.refreshAccountToken(account, true); err != nil {
				result.Status = "error"
				result.Error = err.Error()
				unhealthy++
				logger.Warnf("[HealthCheck] Token 已过期且刷新失败 %s: %v", account.Email, err)
			} else {
				result.Status = "refreshed"
				healthy++
				logger.Infof("[HealthCheck] 过期 Token 已刷新: %s", account.Email)
			}
		} else if account.ExpiresAt > 0 && now > account.ExpiresAt-warmupTokenWindowSeconds {
			// Token 即将过期（15 分钟内）：主动预刷新。
			// 必须用 force=true：非强制模式只在 2 分钟窗口内才会真正刷新。
			// 刷新成功后 ExpiresAt 会跳到数小时后，自然退出该窗口，不会反复刷新。
			if err := h.refreshAccountToken(account, true); err != nil {
				result.Status = "error"
				result.Error = err.Error()
				unhealthy++
				logger.Warnf("[HealthCheck] Token 预刷新失败 %s: %v", account.Email, err)
			} else {
				result.Status = "refreshed"
				healthy++
				logger.Infof("[HealthCheck] Token 预刷新成功: %s", account.Email)
			}
		} else {
			result.Status = "ok"
			healthy++
		}

		results = append(results, result)
	}

	hc.mu.Lock()
	hc.LastRunAt = now
	hc.TotalChecked = len(results)
	hc.Healthy = healthy
	hc.Unhealthy = unhealthy
	hc.Skipped = skipped
	hc.Results = results
	hc.mu.Unlock()

	logger.Infof("[HealthCheck] 完成: %d 正常, %d 异常, %d 跳过", healthy, unhealthy, skipped)
}

// apiGetHealthStatus GET /admin/api/health-check
func (h *Handler) apiGetHealthStatus(w http.ResponseWriter, r *http.Request) {
	hc := h.hc
	hc.mu.RLock()
	resp := map[string]interface{}{
		"lastRunAt":    hc.LastRunAt,
		"nextRunAt":    hc.NextRunAt,
		"running":      hc.Running,
		"totalChecked": hc.TotalChecked,
		"healthy":      hc.Healthy,
		"unhealthy":    hc.Unhealthy,
		"skipped":      hc.Skipped,
		"results":      hc.Results,
	}
	hc.mu.RUnlock()
	json.NewEncoder(w).Encode(resp)
}

// apiTriggerHealthCheck POST /admin/api/health-check/trigger
func (h *Handler) apiTriggerHealthCheck(w http.ResponseWriter, r *http.Request) {
	go h.runHealthCheck()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "health check triggered",
	})
}
