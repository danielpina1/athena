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

func TestWorkerPrompt(t *testing.T) {
	for _, banned := range []string{"orchestrator_", "Feishu", "feishu", "DSH_", "lark-cli", "reply_to_trigger_id", "pr_number"} {
		if strings.Contains(Worker, banned) {
			t.Errorf("worker prompt mentions %q", banned)
		}
	}
	for _, want := range []string{"`report`", "`ack`", "`reply`", "`follow_thread`", "`mark_unrelated`", "`handback`", "`pull`",
		"athena/<ticket id>", "git push -u origin", "gh pr create", "ATHENA_GITHUB_REPOSITORY", "ATHENA_GITHUB_ACCOUNT",
		"ATHENA_GITHUB_COMMIT_EMAIL", "ATHENA_GITHUB_NUMBER", "ATHENA_GITHUB_PR_HEAD", "trigger_id", "600 characters",
		"Discover how the repository builds"} {
		if !strings.Contains(Worker, want) {
			t.Errorf("worker prompt lacks %q", want)
		}
	}
}
