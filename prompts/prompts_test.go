package prompts

import (
	"strings"
	"testing"
)

func TestRouterPrompt(t *testing.T) {
	for _, file := range []string{router, admission, replies, subscriptions} {
		if !strings.HasPrefix(file, "<!-- Adapted from deepseek-ai/dsh-superagent-prototype (MIT). -->\n") {
			t.Errorf("prompt file lacks the MIT header: %.60q", file)
		}
	}
	if strings.Contains(Router, "<!--") {
		t.Error("the composed prompt keeps a comment")
	}
	for _, banned := range []string{"orchestrator_", "Feishu", "feishu", "Harness", "DSH", "dsh-", "bash", "worktree_name"} {
		if strings.Contains(Router, banned) {
			t.Errorf("router prompt mentions %q", banned)
		}
	}
	for _, tool := range []string{"route", "eval_decide", "eval_read", "read", "reply", "request_status", "ticket_retry",
		"trigger_requeue", "thread_unsubscribe", "note"} {
		if !strings.Contains(Router, tool) {
			t.Errorf("router prompt never names %s", tool)
		}
	}
	if !strings.Contains(Router, "Never wait or poll") {
		t.Error("router prompt does not forbid polling")
	}
}
