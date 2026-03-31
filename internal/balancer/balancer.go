package balancer

import (
	"gemini-web2api/internal/gemini"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxFailCount 连续失败超过此次数后进入冷却
	MaxFailCount = 3
	// CooldownDuration 冷却持续时间
	CooldownDuration = 5 * time.Minute
)

type AccountEntry struct {
	Client        *gemini.Client
	AccountID     string
	ProxyURL      string
	failCount     int
	cooldownUntil time.Time
}

// IsHealthy 返回该账号是否处于可用状态（未在冷却期内）
func (e *AccountEntry) IsHealthy() bool {
	return time.Now().After(e.cooldownUntil)
}

type AccountPool struct {
	entries []AccountEntry
	index   uint64
	mu      sync.RWMutex
}

func NewAccountPool() *AccountPool {
	return &AccountPool{
		entries: make([]AccountEntry, 0),
	}
}

func (p *AccountPool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = make([]AccountEntry, 0)
	atomic.StoreUint64(&p.index, 0)
}

func (p *AccountPool) Add(client *gemini.Client, accountID string, proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, AccountEntry{
		Client:    client,
		AccountID: accountID,
		ProxyURL:  proxyURL,
	})
}

// Next 按轮询顺序选取下一个健康账号。
// 若所有账号均在冷却中，则返回冷却最早到期的账号（降级兜底）。
func (p *AccountPool) Next() (*gemini.Client, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.entries) == 0 {
		return nil, ""
	}

	n := uint64(len(p.entries))
	start := atomic.AddUint64(&p.index, 1) - 1

	// 第一轮：寻找健康账号
	for i := uint64(0); i < n; i++ {
		idx := (start + i) % n
		if p.entries[idx].IsHealthy() {
			return p.entries[idx].Client, p.entries[idx].AccountID
		}
	}

	// 兜底：所有账号都在冷却，返回冷却最早到期的账号
	log.Printf("[Balancer] All accounts are in cooldown, falling back to earliest recovery account")
	best := 0
	for i := 1; i < len(p.entries); i++ {
		if p.entries[i].cooldownUntil.Before(p.entries[best].cooldownUntil) {
			best = i
		}
	}
	return p.entries[best].Client, p.entries[best].AccountID
}

// MarkFailed 标记指定账号发生了一次失败。
// 连续失败 MaxFailCount 次后，账号进入 CooldownDuration 的冷却期，并重置计数。
func (p *AccountPool) MarkFailed(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.entries {
		if p.entries[i].AccountID == accountID {
			p.entries[i].failCount++
			if p.entries[i].failCount >= MaxFailCount {
				p.entries[i].cooldownUntil = time.Now().Add(CooldownDuration)
				p.entries[i].failCount = 0
				log.Printf("[Balancer] Account '%s' entered cooldown for %v after %d consecutive failures",
					accountID, CooldownDuration, MaxFailCount)
			}
			return
		}
	}
}

// MarkSuccess 标记指定账号请求成功，重置失败计数。
func (p *AccountPool) MarkSuccess(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.entries {
		if p.entries[i].AccountID == accountID {
			p.entries[i].failCount = 0
			p.entries[i].cooldownUntil = time.Time{}
			return
		}
	}
}

func (p *AccountPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

func (p *AccountPool) ReplaceAccounts(newAccountIDs []string, changedEntries map[string]AccountEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	oldEntries := make(map[string]AccountEntry)
	for _, entry := range p.entries {
		oldEntries[entry.AccountID] = entry
	}

	p.entries = make([]AccountEntry, 0, len(newAccountIDs))
	for _, accountID := range newAccountIDs {
		if newEntry, changed := changedEntries[accountID]; changed {
			// Cookie 已变更，重置健康状态
			newEntry.failCount = 0
			newEntry.cooldownUntil = time.Time{}
			p.entries = append(p.entries, newEntry)
		} else if oldEntry, existed := oldEntries[accountID]; existed {
			// Cookie 未变更，保留现有健康状态
			p.entries = append(p.entries, oldEntry)
		}
	}
}
