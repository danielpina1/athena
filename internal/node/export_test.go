package node

import "testing"

// For the tests of package node_test, which run the node with the fleet and the node link (a package node cannot
// import): the stand-ins of the internal tests.

// FakeGitHubForTest points the node's https://github.com/o/r at a local bare repository (fakeGitHub) until the test
// ends, and returns a clone of it.
func FakeGitHubForTest(t *testing.T) (work string) {
	_, work = fakeGitHub(t, "o/r")
	return work
}

// FakeCredsForTest is a Credentials that hands out fake tokens and knows the bot's identity.
func FakeCredsForTest() Credentials { return &fakeCreds{} }

// FakeClaudeForTest is the fake claude binary; ClaudeFixtureForTest a fixture's absolute path.
func FakeClaudeForTest(t *testing.T) string { return fakeClaude(t) }

func ClaudeFixtureForTest(t *testing.T, name string) string { return claudeFixture(t, name) }
