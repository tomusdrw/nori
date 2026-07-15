package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseServiceFormReadsCompleteEnvFile(t *testing.T) {
	body := "name=app&watched_image=ghcr.io%2Fme%2Fapp%3Alatest&policy=manual&deploy_script=echo+ok&env_file=PORT%3D8080%0ASECRET%3D%22hello+world%22%0A"
	req := httptest.NewRequest("POST", "/services", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	form := parseServiceForm(req)
	if form.EnvFile != "PORT=8080\nSECRET=\"hello world\"\n" {
		t.Fatalf("EnvFile = %q", form.EnvFile)
	}
}

func TestValidateServiceFormRejectsInvalidBash(t *testing.T) {
	form := ServiceFormData{EnvFile: "PORT=8080\n", DeployScript: "if true; then\n  echo broken"}
	err := validateServiceForm(context.Background(), form)
	if err == nil || !strings.Contains(err.Error(), "deploy script") {
		t.Fatalf("expected deploy script validation error, got %v", err)
	}
}

func TestValidateServiceFormRejectsInvalidDotenv(t *testing.T) {
	form := ServiceFormData{EnvFile: "NOT VALID", DeployScript: "echo ok"}
	err := validateServiceForm(context.Background(), form)
	if err == nil || !strings.Contains(err.Error(), "environment file") {
		t.Fatalf("expected environment file validation error, got %v", err)
	}
}

func TestEditorValidationEndpointReturnsBashDiagnostic(t *testing.T) {
	body := "kind=bash&content=if+true%3B+then%0A++echo+broken"
	req := httptest.NewRequest("POST", "/validate/editor", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	(&Server{}).handleEditorValidate(rr, req)

	var result editorValidation
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Valid || result.Line == 0 || !strings.Contains(result.Message, "bash syntax") {
		t.Fatalf("unexpected validation result: %+v", result)
	}
}
