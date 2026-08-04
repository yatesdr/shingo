package notify

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingocore/config"
)

type Notifier struct {
	mu       sync.RWMutex
	cfg      *config.NotificationsConfig
	throttle map[string]time.Time
}

func New(cfg *config.NotificationsConfig) *Notifier {
	return &Notifier{
		cfg:      cfg,
		throttle: make(map[string]time.Time),
	}
}

func (n *Notifier) Enabled() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cfg.Enabled && n.cfg.SMTPHost != "" && n.cfg.FromAddress != "" && len(n.cfg.Recipients) > 0
}

func (n *Notifier) Reconfigure(cfg *config.NotificationsConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cfg = cfg
	n.throttle = make(map[string]time.Time)
}

func (n *Notifier) Config() *config.NotificationsConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cfg
}

func (n *Notifier) Send(subject, body string) error {
	n.mu.Lock()
	cfg := n.cfg
	now := time.Now()
	deadline := now.Add(-time.Duration(cfg.ThrottleMinutes) * time.Minute)

	suppressed := 0
	for _, r := range cfg.Recipients {
		if last, ok := n.throttle[r]; ok && last.After(deadline) {
			suppressed++
			continue
		}
		n.throttle[r] = now
	}
	n.mu.Unlock()

	if suppressed == len(cfg.Recipients) {
		return nil
	}

	from := cfg.FromAddress
	to := make([]string, 0, len(cfg.Recipients))
	for _, r := range cfg.Recipients {
		if last, ok := n.throttle[r]; ok && last.Before(deadline.Add(time.Second)) {
			delete(n.throttle, r)
		}
		to = append(to, r)
	}

	if len(to) == 0 {
		return nil
	}

	n.mu.RLock()
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	n.mu.RUnlock()

	sendMail := PlainSend
	if cfg.SMTPTLS {
		sendMail = TLSSend
	}

	err := sendMail(addr, cfg.SMTPUser, cfg.SMTPPassword, from, to, subject, body)
	if err != nil {
		log.Printf("notify: send error: %v", err)
		return err
	}

	log.Printf("notify: alert sent to %d recipient(s)", len(to))
	return nil
}
