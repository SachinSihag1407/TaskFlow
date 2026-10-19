package handlers

import "github.com/taskflow/taskflow/internal/db"

// Builtin returns a registry with every built-in handler.
func Builtin(database db.DBTX, allowPrivateWebhooks bool, webhookMaxBody int64) *Registry {
	return NewRegistry(
		Report{},
		Batch{},
		Notify{DB: database},
		NewWebhook(allowPrivateWebhooks, webhookMaxBody),
		Sleep{},
	)
}
