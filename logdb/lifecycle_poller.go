package logdb

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// SQSAPI is the subset of the SQS client the poller needs.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// LifecyclePoller consumes ASG launch-hook events from an SQS queue
// and dispatches them to a LifecycleHandler. One poller per queue;
// the SNS fan-out upstream merges all three tier hooks into the
// same queue so we only need one poller.
type LifecyclePoller struct {
	handler  *LifecycleHandler
	sqs      SQSAPI
	queueURL string

	// Wait/poll tunables. Exported for tests.
	WaitTimeSeconds   int32
	VisibilityTimeout int32
}

// NewLifecyclePoller builds the production poller with the default
// long-poll settings (20s wait / 60s visibility).
func NewLifecyclePoller(handler *LifecycleHandler, sqsClient SQSAPI, queueURL string) *LifecyclePoller {
	return &LifecyclePoller{
		handler:           handler,
		sqs:               sqsClient,
		queueURL:          queueURL,
		WaitTimeSeconds:   20,
		VisibilityTimeout: 60,
	}
}

// BuildLifecyclePoller wires a poller from a region + queue URL,
// creating AWS clients of its own. Returns nil, nil when queueURL is
// empty so callers can no-op in dev.
func BuildLifecyclePoller(ctx context.Context, region, queueURL string, cfg LifecycleHandlerConfig) (*LifecyclePoller, error) {
	if queueURL == "" {
		return nil, nil
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	if cfg.Volumes == nil {
		cfg.Volumes = NewEC2VolumeManagerWithClient(ec2.NewFromConfig(awsCfg))
	}
	if cfg.EC2Instances == nil {
		cfg.EC2Instances = ec2.NewFromConfig(awsCfg)
	}
	if cfg.ASG == nil {
		cfg.ASG = autoscaling.NewFromConfig(awsCfg)
	}
	handler := NewLifecycleHandler(cfg)
	return NewLifecyclePoller(handler, sqs.NewFromConfig(awsCfg), queueURL), nil
}

// Start spawns the poll loop. Returns immediately; the loop exits
// when ctx is cancelled.
func (p *LifecyclePoller) Start(ctx context.Context) {
	go func() {
		log.Printf("lifecycle: polling %s", p.queueURL)
		for {
			if ctx.Err() != nil {
				return
			}
			p.pollOnce(ctx)
		}
	}()
}

// pollOnce receives up to one batch of messages, hands each to the
// handler, and deletes on success. A handler error leaves the
// message in the queue so it'll be retried after the visibility
// timeout — which is the right behavior for transient attach
// failures.
func (p *LifecyclePoller) pollOnce(ctx context.Context) {
	out, err := p.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(p.queueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     p.WaitTimeSeconds,
		VisibilityTimeout:   p.VisibilityTimeout,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("lifecycle: ReceiveMessage: %v", err)
		time.Sleep(5 * time.Second)
		return
	}
	for _, msg := range out.Messages {
		body := []byte("")
		if msg.Body != nil {
			body = []byte(*msg.Body)
		}
		ev, err := ParseLifecycleEvent(body)
		if err != nil {
			log.Printf("lifecycle: parse: %v", err)
			// Malformed — delete so we don't loop on it.
			p.deleteMessage(ctx, msg.ReceiptHandle)
			continue
		}
		if _, err := p.handler.HandleLaunchEvent(ctx, ev); err != nil {
			log.Printf("lifecycle: handle %s: %v", ev.InstanceID, err)
			// Abandoned in-handler; delete to stop retrying.
			// (ASG will launch a replacement instance on ABANDON.)
			p.deleteMessage(ctx, msg.ReceiptHandle)
			continue
		}
		p.deleteMessage(ctx, msg.ReceiptHandle)
	}
}

func (p *LifecyclePoller) deleteMessage(ctx context.Context, handle *string) {
	if handle == nil {
		return
	}
	_, err := p.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(p.queueURL),
		ReceiptHandle: handle,
	})
	if err != nil {
		log.Printf("lifecycle: DeleteMessage: %v", err)
	}
}
