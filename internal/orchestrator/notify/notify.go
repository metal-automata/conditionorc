package notify

import (
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	"github.com/sirupsen/logrus"
)

// XXX: This module assumes that eventually we'll want to have other targets for
// notifications aside Slack, either singly (as in either Slack or some other thing)
// or in some combination (that is, Slack *and* some other thing). For the "I only
// want to send a Slack message to a channel and or a person" this is overkill.

type NotificationType string

const (
	Slack NotificationType = "slack"
	Null  NotificationType = "null"
)

type Configuration struct {
	Enabled          bool `mapstructure:"enabled"`
	NotificationType `mapstructure:"type"`
	Channel          string `mapstructure:"channel"`
	Token            string `mapstructure:"token"`
}

type Sender interface {
	Send(upd *v1types.ConditionUpdateEvent) error
}

type nullNotifier struct{}

func (n *nullNotifier) Send(_ *v1types.ConditionUpdateEvent) error {
	return nil
}

func New(l *logrus.Logger, cfg Configuration) Sender {
	var notifier Sender = &nullNotifier{}

	if !cfg.Enabled {
		return notifier
	}

	switch cfg.NotificationType {
	case Null:
	case Slack:
		notifier = newSlackSender(l, cfg)
	default:
		l.WithField("configured_type", cfg.NotificationType).
			Warn("unsupported notifier type -- using no-op notifier")
	}
	return notifier
}
