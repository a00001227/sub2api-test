package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type fakeBedrockProber struct {
	err     error
	calls   int
	lastKey string
	lastReg string
}

func (f *fakeBedrockProber) ProbeBedrockCredential(_ context.Context, apiKey, region string) error {
	f.calls++
	f.lastKey, f.lastReg = apiKey, region
	return f.err
}

const testBedrockKey = "ABSKQmVkcm9ja0FQSUtleS10ZXN0LWF0LTEyMzQ1Njc4OTAxMjpUZXN0VGVzdFRlc3Q"

func TestParseBedrockImportCredential(t *testing.T) {
	cases := []struct {
		in      string
		key     string
		region  string
		wantErr string
	}{
		{in: testBedrockKey, key: testBedrockKey, region: "us-east-1"},
		{in: testBedrockKey + "|us-east-2", key: testBedrockKey, region: "us-east-2"},
		{in: testBedrockKey + " @ EU-WEST-1 ", key: testBedrockKey, region: "eu-west-1"},
		{in: "AKIAIOSFODNN7EXAMPLE|us-east-1", wantErr: "IAM access key"},
		{in: "sk-ant-sid01-xxx", wantErr: "format invalid"},
		{in: testBedrockKey + "|mars", wantErr: "aws region"},
		{in: testBedrockKey + "|us-east-1|extra", wantErr: "one line"},
	}
	for _, c := range cases {
		k, r, err := parseBedrockImportCredential(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%q: want err containing %q, got %v", c.in, c.wantErr, err)
			}
			continue
		}
		if err != nil || k != c.key || r != c.region {
			t.Fatalf("%q: got (%q,%q,%v)", c.in, k, r, err)
		}
	}
}

func TestImportBedrock_Success_NoProxy_TypeBedrock(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	// 分配器给了代理也不该用:bedrock 直连 AWS。
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 7, Name: "us-1", Status: StatusActive}}, nil)
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, &fakeCookieAuth{token: &TokenInfo{AccessToken: "x"}}, wh)
	probe := &fakeBedrockProber{}
	svc.bedrockProbe = probe

	res, err := svc.ImportCredential(context.Background(), ImportCredentialInput{
		ExternalProviderAccountID: "pa_bedrock1",
		ProviderType:              "bedrock",
		Credential:                testBedrockKey + "|us-east-2",
		Region:                    "us",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != "active" {
		t.Fatalf("status=%s", res.Status)
	}
	if probe.calls != 1 || probe.lastReg != "us-east-2" || probe.lastKey != testBedrockKey {
		t.Fatalf("probe not called as expected: %+v", probe)
	}
	in := accounts.lastCreate
	if in == nil {
		t.Fatal("account not created")
	}
	if in.Type != AccountTypeBedrock || in.Platform != PlatformAnthropic {
		t.Fatalf("type/platform = %s/%s", in.Type, in.Platform)
	}
	if in.ProxyID != nil {
		t.Fatalf("bedrock must not bind a proxy, got %v", *in.ProxyID)
	}
	if in.Credentials["auth_mode"] != "apikey" || in.Credentials["api_key"] != testBedrockKey || in.Credentials["aws_region"] != "us-east-2" {
		t.Fatalf("credentials = %v", in.Credentials)
	}
	if len(wh.sent) != 1 || !strings.HasPrefix(wh.sent[0].Plan, "aws_bedrock_") || wh.sent[0].ProviderType != "bedrock" {
		t.Fatalf("webhook: %+v", wh.sent)
	}
}

func TestImportBedrock_ProbeFails_NoAccount(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	svc := newImportSvc(accounts, NewProxyAllocator(&fakeAllocationRepo{}, nil), &fakeCookieAuth{}, &fakeWebhookNotifier{})
	svc.bedrockProbe = &fakeBedrockProber{err: errors.New("AWS 403: Bedrock rejected the API key")}

	_, err := svc.ImportCredential(context.Background(), ImportCredentialInput{
		ExternalProviderAccountID: "pa_bedrock2",
		ProviderType:              "bedrock",
		Credential:                testBedrockKey,
		Region:                    "us",
	})
	if err == nil {
		t.Fatal("want error")
	}
	var ie *infraerrors.ApplicationError
	if !errors.As(err, &ie) || ie.Reason != "INVALID_CREDENTIAL" || !strings.Contains(ie.Message, "AWS 403") {
		t.Fatalf("got %v", err)
	}
	if accounts.lastCreate != nil {
		t.Fatal("account must not be created when probe fails")
	}
}

func TestImportBedrock_BadFormat_NoProbe(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	svc := newImportSvc(accounts, NewProxyAllocator(&fakeAllocationRepo{}, nil), &fakeCookieAuth{}, &fakeWebhookNotifier{})
	probe := &fakeBedrockProber{}
	svc.bedrockProbe = probe
	_, err := svc.ImportCredential(context.Background(), ImportCredentialInput{
		ExternalProviderAccountID: "pa_bedrock3",
		ProviderType:              "bedrock",
		Credential:                "AKIAIOSFODNN7EXAMPLE",
		Region:                    "us",
	})
	if err == nil || !strings.Contains(err.Error(), "IAM access key") {
		t.Fatalf("got %v", err)
	}
	if probe.calls != 0 || accounts.lastCreate != nil {
		t.Fatal("must fail before probe/create")
	}
}

func TestClassifyBedrockProbeResponse(t *testing.T) {
	if err := classifyBedrockProbeResponse(200, strings.NewReader(""), "m", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if err := classifyBedrockProbeResponse(429, strings.NewReader(`{"message":"Too many requests"}`), "m", "us-east-1"); err != nil {
		t.Fatalf("429 = key valid, got %v", err)
	}
	err := classifyBedrockProbeResponse(403, strings.NewReader(`{"message":"You don't have access to the model with the specified model ID."}`), "us.anthropic.claude-haiku", "us-east-2")
	if err == nil || !strings.Contains(err.Error(), "Model access") {
		t.Fatalf("got %v", err)
	}
	err = classifyBedrockProbeResponse(403, strings.NewReader(`{"Message":"The security token included in the request is invalid."}`), "m", "us-east-1")
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("got %v", err)
	}
}
