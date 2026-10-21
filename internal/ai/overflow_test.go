package ai

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsContextOverflow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"http 413", opsHTTPFailure(http.StatusRequestEntityTooLarge), true},
		{"http 401", opsHTTPFailure(http.StatusUnauthorized), false},
		{"http 429", opsHTTPFailure(http.StatusTooManyRequests), false},
		{"openai code", fmt.Errorf("%w: %s", opsHTTPFailure(400), `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 4096 tokens."}}`), true},
		{"anthropic text", fmt.Errorf("%w: %s", opsHTTPFailure(400), `{"error":{"message":"prompt is too long: 210000 tokens > 200000 maximum"}}`), true},
		{"claude window", fmt.Errorf("%w: %s", opsHTTPFailure(400), "prompt is too long"), true},
		{"chinese overflow", fmt.Errorf("%w: %s", opsHTTPFailure(400), "输入长度超过上下文限制"), true},
		{"rate limit lookalike", fmt.Errorf("%w: %s", opsHTTPFailure(400), "context length exceeded; rate limit enforced"), false},
		{"max_tokens param", fmt.Errorf("%w: %s", opsHTTPFailure(400), "max_tokens must be less than context length"), false},
		{"token_limit finish", opsFailure("token_limit", 0), false},
		{"transport", opsFailure("transport_error", 0), false},
	}
	for _, c := range cases {
		if got := IsContextOverflow(c.err); got != c.want {
			t.Errorf("%s: IsContextOverflow = %v, want %v", c.name, got, c.want)
		}
	}
}
