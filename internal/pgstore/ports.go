package pgstore

import (
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// Verificação em tempo de compilação das portas implementadas.
var (
	_ notifications.Store = (*Notifications)(nil)
	_ notifications.Tx    = notificationsTx{}
	_ collections.Store   = (*Collections)(nil)
	_ collections.Tx      = collectionsTx{}
	_ webhooks.Store      = (*Webhooks)(nil)
	_ webhooks.Tx         = webhooksTx{}
)
