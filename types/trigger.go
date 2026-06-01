package types

// TriggerType identifies the kind of trigger that starts a workflow execution.
type TriggerType = string

const (
	TriggerManual  TriggerType = "manual"
	TriggerWebhook TriggerType = "webhook"
	TriggerCron    TriggerType = "cron"
	TriggerEvent   TriggerType = "event"
	TriggerQueue   TriggerType = "queue"
)

// TriggerDef describes how a workflow is initiated.
type TriggerDef struct {
	Type       TriggerType    `json:"type,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
	Enabled    bool           `json:"enabled,omitempty"` // indicates whether the trigger is active; omitempty omits the false zero-value
}

// Manual returns a TriggerDef for manual (on-demand) workflow invocation.
func Manual() TriggerDef {
	return TriggerDef{
		Type:    TriggerManual,
		Enabled: true,
	}
}

// Webhook returns a TriggerDef that starts a workflow via an HTTP webhook call.
func Webhook(path string, methods []string) TriggerDef {
	return TriggerDef{
		Type: TriggerWebhook,
		Parameters: map[string]any{
			"path":    path,
			"methods": methods,
		},
		Enabled: true,
	}
}

// Cron returns a TriggerDef that starts a workflow on a cron schedule.
func Cron(schedule string) TriggerDef {
	return TriggerDef{
		Type: TriggerCron,
		Parameters: map[string]any{
			"schedule": schedule,
		},
		Enabled: true,
	}
}

// Event returns a TriggerDef that starts a workflow when a named event is emitted.
func Event(source, event string) TriggerDef {
	return TriggerDef{
		Type: TriggerEvent,
		Parameters: map[string]any{
			"source": source,
			"event":  event,
		},
		Enabled: true,
	}
}

// Queue returns a TriggerDef that starts a workflow when a message arrives on a queue.
func Queue(queue string) TriggerDef {
	return TriggerDef{
		Type: TriggerQueue,
		Parameters: map[string]any{
			"queue": queue,
		},
		Enabled: true,
	}
}
