package opschat

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type classifiedAnalysisError string

func (e classifiedAnalysisError) Error() string          { return "private-provider-secret" }
func (e classifiedAnalysisError) OpsFailureCode() string { return string(e) }

func TestAnalysisFailureCodesAreBoundedAndNeverExposeErrors(t *testing.T) {
	for _, code := range []string{"token_limit", "timeout", "rejected", "invalid_response", "unavailable"} {
		if got := analysisFailureText(fmt.Errorf("private: %w", classifiedAnalysisError(code))); got != "[analysis:"+code+"]" {
			t.Fatal(got)
		}
	}
	for _, err := range []error{nil, errors.New("private-provider-secret"), classifiedAnalysisError("private-provider-secret")} {
		if got := analysisFailureText(err); got != "[analysis:unknown]" {
			t.Fatal(got)
		}
	}
	for err, code := range map[error]string{context.DeadlineExceeded: "timeout", ErrQuota: "input_limit", ErrUnavailable: "unavailable"} {
		if got := analysisFailureText(fmt.Errorf("private: %w", err)); got != "[analysis:"+code+"]" {
			t.Fatal(got)
		}
	}
}

// 非模型类哨兵错误也要落到具体可读码,而非笼统 unknown(用户可见原因)。
func TestAnalysisFailureCodesForSentinels(t *testing.T) {
	for err, code := range map[error]string{ErrInvalid: "input_limit", ErrConsent: "rejected", ErrConflict: "unavailable"} {
		if got := analysisFailureText(fmt.Errorf("wrap: %w", err)); got != "[analysis:"+code+"]" {
			t.Fatal(err, got)
		}
	}
}
