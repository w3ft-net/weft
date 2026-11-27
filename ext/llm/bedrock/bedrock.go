// Package bedrock is an ingest.LLM over Amazon Bedrock's Converse API: the model
// behind schema-field classification.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/w3ft-net/weft/ingest"
)

// LLM is an ingest.LLM over Amazon Bedrock's Converse API with tool use, which
// forces structured JSON output instead of free-text parsing. It is the only
// Bedrock code; it moves to ext/llm/bedrock.
type LLM struct {
	client  *bedrockruntime.Client
	modelID string
}

// New builds the LLM against modelID (a Bedrock model ID or
// cross-region inference profile ID/ARN, e.g. Amazon Nova Lite) using cfg's
// credentials and region.
func New(cfg aws.Config, modelID string) *LLM {
	return &LLM{client: bedrockruntime.NewFromConfig(cfg), modelID: modelID}
}

// ClassifierFromEnv builds a classifier over Bedrock from
// BEDROCK_CLASSIFIER_MODEL_ID and BEDROCK_REGION, shared by both the api and the
// coordinator (each constructs one Server that needs a Classifier).
//
// Deliberately separate from AWS_REGION (used elsewhere for SSM/S3/EC2):
// Bedrock model availability is region-specific and Nova Lite may not be
// offered wherever the coordinator otherwise runs. Returns (nil, nil), not an
// error, when BEDROCK_CLASSIFIER_MODEL_ID is unset: classification is then
// disabled (ClassifyAsync treats a nil Classifier as "skip") until an operator
// configures it, rather than the server failing to start.
func ClassifierFromEnv(ctx context.Context) (ingest.Classifier, error) {
	modelID := os.Getenv("BEDROCK_CLASSIFIER_MODEL_ID")
	if modelID == "" {
		return nil, nil
	}
	region := os.Getenv("BEDROCK_REGION")
	if region == "" {
		return nil, fmt.Errorf("BEDROCK_CLASSIFIER_MODEL_ID is set but BEDROCK_REGION is not")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config for bedrock classifier: %w", err)
	}
	return ingest.LLMClassifier{LLM: New(cfg, modelID)}, nil
}

func (b *LLM) CallTool(ctx context.Context, req ingest.ToolRequest) (json.RawMessage, error) {
	resp, err := b.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(b.modelID),
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: req.User}},
		}},
		System: []brtypes.SystemContentBlock{&brtypes.SystemContentBlockMemberText{Value: req.System}},
		ToolConfig: &brtypes.ToolConfiguration{
			Tools: []brtypes.Tool{&brtypes.ToolMemberToolSpec{Value: brtypes.ToolSpecification{
				Name:        aws.String(req.Tool),
				Description: aws.String(req.Description),
				InputSchema: &brtypes.ToolInputSchemaMemberJson{Value: brdoc.NewLazyDocument(req.Schema)},
			}}},
			ToolChoice: &brtypes.ToolChoiceMemberAny{},
		},
		InferenceConfig: &brtypes.InferenceConfiguration{Temperature: aws.Float32(0), MaxTokens: aws.Int32(int32(req.MaxTokens))},
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock converse: %w", err)
	}
	raw, err := extractToolUseInput(resp)
	if err != nil {
		return nil, err
	}
	// Through the document's JSON form: its own UnmarshalSmithyDocument ignores
	// `json` struct tags, so a multi-word key like field_index never reached its
	// field and every proposal came back with nothing to locate it by.
	return raw.MarshalSmithyDocument()
}

// extractToolUseInput pulls the tool_use content block's Input document
// out of a Converse response. ToolChoiceMemberAny forces the model to use
// a tool, so a well-behaved model always returns exactly this shape; if
// it doesn't, that's a hard error rather than something worth guessing at.
func extractToolUseInput(resp *bedrockruntime.ConverseOutput) (brdoc.Interface, error) {
	msg, ok := resp.Output.(*brtypes.ConverseOutputMemberMessage)
	if !ok {
		return nil, fmt.Errorf("unexpected converse output type %T", resp.Output)
	}
	for _, block := range msg.Value.Content {
		if tu, ok := block.(*brtypes.ContentBlockMemberToolUse); ok {
			return tu.Value.Input, nil
		}
	}
	return nil, fmt.Errorf("model response had no tool_use block (stop reason %s)", resp.StopReason)
}
