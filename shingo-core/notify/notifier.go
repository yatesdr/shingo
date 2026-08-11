package notify

import (
	"fmt"
	"log"
	"sync"

	"shingocore/config"
)

type Notifier struct {
	mu  sync.RWMutex
	cfg *config.NotificationsConfig
}

func New(cfg *config.NotificationsConfig) *Notifier {
	return &Notifier{
		cfg: cfg,
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
}

func (n *Notifier) Config() *config.NotificationsConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cfg
}

func (n *Notifier) Send(subject, body string) error {
	n.mu.RLock()
	cfg := n.cfg
	n.mu.RUnlock()

	if !cfg.Enabled || cfg.SMTPHost == "" || cfg.FromAddress == "" || len(cfg.Recipients) == 0 {
		return fmt.Errorf("notifications not configured")
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	sendMail := PlainSend
	if cfg.SMTPTLS {
		sendMail = TLSSend
	}

	err := sendMail(addr, cfg.SMTPUser, cfg.SMTPPassword, cfg.FromAddress, cfg.Recipients, subject, body)
	if err != nil {
		log.Printf("notify: send error: %v", err)
		return err
	}

	log.Printf("notify: alert sent to %d recipient(s)", len(cfg.Recipients))
	return nil
}

func (n *Notifier) SendWithHeaders(subject, body string, opts ...SendOption) error {
	n.mu.RLock()
	cfg := n.cfg
	n.mu.RUnlock()

	if !cfg.Enabled || cfg.SMTPHost == "" || cfg.FromAddress == "" || len(cfg.Recipients) == 0 {
		return fmt.Errorf("notifications not configured")
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	sendMail := PlainSend
	if cfg.SMTPTLS {
		sendMail = TLSSend
	}

	err := sendMail(addr, cfg.SMTPUser, cfg.SMTPPassword, cfg.FromAddress, cfg.Recipients, subject, body, opts...)
	if err != nil {
		log.Printf("notify: send error: %v", err)
		return err
	}

	log.Printf("notify: alert sent to %d recipient(s)", len(cfg.Recipients))
	return nil
}
