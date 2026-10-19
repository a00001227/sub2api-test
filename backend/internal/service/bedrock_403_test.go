package service

import "testing"

func TestIsBedrockCredentialRejected(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"message":"Authentication failed: Please make sure your API Key is valid."}`, true},
		{`{"Message":"The security token included in the request is invalid."}`, true},
		{`{"message":"anthropic.claude-fable-5-1 is not available for this account. You can explore other available models on Amazon Bedrock."}`, false},
		{`{"message":"You don't have access to the model with the specified model ID."}`, false},
		{`{"message":"User is not authorized to perform: bedrock:InvokeModel on resource: arn:aws:bedrock:us-east-2::foundation-model/anthropic.claude-opus-5"}`, false},
	}
	for _, c := range cases {
		if got := isBedrockCredentialRejected("", []byte(c.body)); got != c.want {
			t.Fatalf("%s: got %v want %v", c.body, got, c.want)
		}
	}
}
