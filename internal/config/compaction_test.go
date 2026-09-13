package config_test

import (
	"fmt"
	"github.com/zydtiger/codex-model-router/internal/config"
	"testing"
)

func TestCompactionConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
		limit int
	}{
		{`{}`, true, 0}, {`{"adapter":"text_summary"}`, true, 4096}, {`{"adapter":"text_summary","max_output_tokens":8192}`, true, 8192},
		{`{"adapter":"other"}`, false, 0}, {`{"max_output_tokens":4096}`, false, 0}, {`{"adapter":"text_summary","max_output_tokens":255}`, false, 0}, {`{"adapter":"text_summary","max_output_tokens":32769}`, false, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := config.Parse([]byte(fmt.Sprintf(`{"routes":[{"name":"test","base_url":"http://127.0.0.1:4011","models":["test"],"compaction":%s}]}`, tc.value)))
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
			if tc.valid && cfg.Routes[0].Compaction.MaxOutputTokens != tc.limit {
				t.Fatal(cfg.Routes[0].Compaction)
			}
		})
	}
}
