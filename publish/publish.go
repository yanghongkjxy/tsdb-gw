package publish

import (
	"github.com/raintank/tsdb-gw/metrics_client"
	log "github.com/sirupsen/logrus"
	schema "gopkg.in/raintank/schema.v1"
)

type Publisher interface {
	Publish(metrics []*schema.MetricData) error
	Type() string
}

var (
	publisher Publisher

	// Persister allows pushing metrics to the Persistor Service
	Persistor *metrics_client.Client
)

func Init(p Publisher) {
	if p == nil {
		publisher = &nullPublisher{}
	} else {
		publisher = p
	}
	log.Infof("using %s publisher", publisher.Type())
}

func Publish(metrics []*schema.MetricData) error {
	return publisher.Publish(metrics)
}

// nullPublisher drops all metrics passed through the publish interface
type nullPublisher struct{}

func (*nullPublisher) Publish(metrics []*schema.MetricData) error {
	log.Debugf("publishing not enabled, dropping %d metrics", len(metrics))
	return nil
}

func (*nullPublisher) Type() string {
	return "nullPublisher"
}

func Persist(metrics []*schema.MetricData) error {
	return publisher.Publish(metrics)
}
