package config

import "strings"

// Rabbit holds the RabbitMQ connection setting of the worker (spec 9.11, 13.2.5). The vhost is part
// of the URL (local: "/", staging/prod: "/kotoki" written %2Fkotoki).
type Rabbit struct {
	URL Secret `env:"RABBITMQ_URL"`
}

func defaultRabbit() Rabbit {
	return Rabbit{URL: "amqp://guest:guest@localhost:5672/"} //nolint:gosec // G101: spec default for local dev
}

func (r Rabbit) validate(v *validator, c Common) {
	switch {
	case r.URL == "":
		v.fail("RABBITMQ_URL", "required")
	case c.strict() && !strings.HasPrefix(r.URL.Reveal(), "amqps://"):
		v.fail("RABBITMQ_URL", "must use amqps:// when APP_ENV is staging or prod")
	case !strings.HasPrefix(r.URL.Reveal(), "amqp://") && !strings.HasPrefix(r.URL.Reveal(), "amqps://"):
		v.fail("RABBITMQ_URL", "must start with amqp:// or amqps://")
	}
}
