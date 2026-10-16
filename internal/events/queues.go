package events

// Queue names (spec 9.6.2). Each is also the consumer name in processed_events and the routing key
// that dead-letters back from kotoki.requeue.
const (
	QueueEvaluation = "evaluation.session_completed"
	QueueMemory     = "memory.session_completed"
	QueueSummary    = "summary.session_completed"
	QueueLearner    = "learner.evaluation_completed"
)
