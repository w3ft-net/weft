package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// CloudWatchFetchOpts parameterizes a single FilterLogEvents call (plus
// pagination) against a customer's CloudWatch log group.
type CloudWatchFetchOpts struct {
	Region     string
	RoleARN    string
	ExternalID string
	LogGroup   string
	StartTime  int64  // unix ms; 0 = "1 hour ago"
	EndTime    int64  // unix ms; 0 = "now"
	NextToken  string // continue from a prior Fetch's FetchResult.NextToken
	Limit      int    // stop after this many events; 0 = paginate until exhausted
	// SessionSuffix is appended to the RoleSessionName so trail logs
	// distinguish poller pulls from sample pulls.
	SessionSuffix string
}

// CloudWatchEvent is a single event as returned by CloudWatch, trimmed and
// shaped for weft consumers. Empty messages are dropped by Fetch so callers
// can treat this slice as "useful lines only."
type CloudWatchEvent struct {
	Message   string
	Timestamp time.Time
}

// CloudWatchFetchResult carries the events pulled and a NextToken for
// continuation. NextToken is "" when CloudWatch reported no more pages.
type CloudWatchFetchResult struct {
	Events    []CloudWatchEvent
	NextToken string
}

// FetchCloudWatch assumes the customer's cross-account role and pulls events
// from their CloudWatch log group. Used by both the continuous poller and
// the one-shot sampler behind the "Add CloudWatch source" UI — extracting
// the assume-role + paginate loop out of the poller means we don't risk the
// two paths drifting on things like session names or backoff semantics.
//
// Stops at Limit events (if > 0) or at the end of pagination, whichever
// comes first.
func FetchCloudWatch(ctx context.Context, opts CloudWatchFetchOpts) (*CloudWatchFetchResult, error) {
	if opts.LogGroup == "" {
		return nil, fmt.Errorf("log group required")
	}
	if opts.RoleARN == "" {
		return nil, fmt.Errorf("role arn required")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(opts.Region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	sessionName := "weft"
	if opts.SessionSuffix != "" {
		sessionName = "weft-" + opts.SessionSuffix
	}
	stsClient := sts.NewFromConfig(cfg)
	creds := stscreds.NewAssumeRoleProvider(stsClient, opts.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		if opts.ExternalID != "" {
			o.ExternalID = aws.String(opts.ExternalID)
		}
		o.RoleSessionName = sessionName
	})
	assumedCfg := cfg.Copy()
	assumedCfg.Credentials = aws.NewCredentialsCache(creds)

	client := cloudwatchlogs.NewFromConfig(assumedCfg)

	startTime := opts.StartTime
	if startTime == 0 {
		startTime = time.Now().Add(-1 * time.Hour).UnixMilli()
	}
	endTime := opts.EndTime
	if endTime == 0 {
		endTime = time.Now().UnixMilli()
	}

	var events []CloudWatchEvent
	var nextToken *string
	if opts.NextToken != "" {
		nextToken = aws.String(opts.NextToken)
	}

	for {
		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: aws.String(opts.LogGroup),
			StartTime:    aws.Int64(startTime),
			EndTime:      aws.Int64(endTime),
			NextToken:    nextToken,
		}

		result, err := client.FilterLogEvents(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("FilterLogEvents: %w", err)
		}

		for _, event := range result.Events {
			msg := strings.TrimSpace(aws.ToString(event.Message))
			if msg == "" {
				continue
			}
			events = append(events, CloudWatchEvent{
				Message:   msg,
				Timestamp: time.UnixMilli(aws.ToInt64(event.Timestamp)),
			})
			if opts.Limit > 0 && len(events) >= opts.Limit {
				return &CloudWatchFetchResult{
					Events:    events,
					NextToken: aws.ToString(result.NextToken),
				}, nil
			}
		}

		if result.NextToken == nil {
			return &CloudWatchFetchResult{Events: events, NextToken: ""}, nil
		}
		nextToken = result.NextToken
	}
}
