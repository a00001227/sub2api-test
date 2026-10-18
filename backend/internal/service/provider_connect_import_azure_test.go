package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAzureProber struct {
	err      error
	calls    int
	lastBase string
	lastKey  string
}

func (f *fakeAzureProber) ProbeAzureCredential(_ context.Context, baseURL, apiKey string) error {
	f.calls++
	f.lastBase, f.lastKey = baseURL, apiKey
	return f.err
}

const testAzureKey = "0123456789abcdef0123456789abcdef"

func TestParseAzureImportCredential(t *testing.T) {
	cases := []struct {
		in      string
		base    string
		wantErr string
	}{
		{in: "https://myres.openai.azure.com|" + testAzureKey, base: "https://myres.openai.azure.com/openai/v1"},
		{in: "https://myres.openai.azure.com/|" + testAzureKey, base: "https://myres.openai.azure.com/openai/v1"},
		{in: "https://myres.openai.azure.com/openai/v1 " + testAzureKey, base: "https://myres.openai.azure.com/openai/v1"},
		{in: testAzureKey + "|https://myres.cognitiveservices.azure.com", base: "https://myres.cognitiveservices.azure.com/openai/v1"},
		{in: "https://myres.openai.azure.com/openai/deployments/gpt|" + testAzureKey, wantErr: "path"},
		{in: "https://api.openai.com|" + testAzureKey, wantErr: "not an Azure OpenAI resource"},
		{in: "http://myres.openai.azure.com|" + testAzureKey, wantErr: "https"},
		{in: "https://myres.openai.azure.com|short", wantErr: "api-key looks invalid"},
		{in: testAzureKey, wantErr: "one line"},
	}
	for _, c := range cases {
		base, key, err := parseAzureImportCredential(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%q: want err containing %q, got %v", c.in, c.wantErr, err)
			}
			continue
		}
		if err != nil || base != c.base || key != testAzureKey {
			t.Fatalf("%q: got (%q,%q,%v)", c.in, base, key, err)
		}
	}
}

func TestImportAzure_Success_APIKeyAccount_NoProxy(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	alloc := NewProxyAllocator(&fakeAllocationRepo{proxy: &Proxy{ID: 7, Name: "us-1", Status: StatusActive}}, nil)
	wh := &fakeWebhookNotifier{enabled: true}
	svc := newImportSvc(accounts, alloc, &fakeCookieAuth{token: &TokenInfo{AccessToken: "x"}}, wh)
	probe := &fakeAzureProber{}
	svc.azureProbe = probe

	res, err := svc.ImportCredential(context.Background(), ImportCredentialInput{
		ExternalProviderAccountID: "pa_azure1",
		ProviderType:              "azure",
		Credential:                "https://myres.openai.azure.com|" + testAzureKey,
		Region:                    "us",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != "active" {
		t.Fatalf("status=%s", res.Status)
	}
	if probe.calls != 1 || probe.lastBase != "https://myres.openai.azure.com/openai/v1" || probe.lastKey != testAzureKey {
		t.Fatalf("probe: %+v", probe)
	}
	in := accounts.lastCreate
	if in == nil || in.Type != AccountTypeAPIKey || in.Platform != PlatformOpenAI {
		t.Fatalf("create = %+v", in)
	}
	if in.ProxyID != nil {
		t.Fatal("azure must not bind a proxy")
	}
	if in.Credentials["api_key"] != testAzureKey || in.Credentials["base_url"] != "https://myres.openai.azure.com/openai/v1" {
		t.Fatalf("credentials = %v", in.Credentials)
	}
	if len(wh.sent) != 1 || wh.sent[0].ProviderType != "azure" || wh.sent[0].Plan != "azure_openai_myres.openai.azure.com" {
		t.Fatalf("webhook: %+v", wh.sent)
	}
}

func TestImportAzure_ProbeFails_NoAccount(t *testing.T) {
	accounts := newFakeConnectAccountRepo()
	svc := newImportSvc(accounts, NewProxyAllocator(&fakeAllocationRepo{}, nil), &fakeCookieAuth{}, &fakeWebhookNotifier{})
	svc.azureProbe = &fakeAzureProber{err: errors.New("Azure 401: api-key rejected")}
	_, err := svc.ImportCredential(context.Background(), ImportCredentialInput{
		ExternalProviderAccountID: "pa_azure2",
		ProviderType:              "azure",
		Credential:                "https://myres.openai.azure.com|" + testAzureKey,
		Region:                    "us",
	})
	if err == nil || !strings.Contains(err.Error(), "Azure 401") {
		t.Fatalf("got %v", err)
	}
	if accounts.lastCreate != nil {
		t.Fatal("account must not be created when probe fails")
	}
}

func TestAzureErrorMessage(t *testing.T) {
	if got := azureErrorMessage([]byte(`{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`)); !strings.HasPrefix(got, "DeploymentNotFound: ") {
		t.Fatalf("got %q", got)
	}
	if got := azureErrorMessage([]byte(`{"message":"Unauthorized"}`)); got != "Unauthorized" {
		t.Fatalf("got %q", got)
	}
}
