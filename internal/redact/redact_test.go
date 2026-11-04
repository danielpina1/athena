package redact

import "testing"

func TestString(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		// NAME=value with a secret-looking name, matched on word boundaries.
		{"env token", "GITHUB_TOKEN=ghp_abc123 make release", "GITHUB_TOKEN=*** make release"},
		{"export api key", "export API_KEY=sk-live-1", "export API_KEY=***"},
		{"quoted value", `DB_PASSWORD="hunter 2" ./run`, `DB_PASSWORD=*** ./run`},
		{"single quoted", `AWS_SECRET_ACCESS_KEY='a/b+c' aws s3 ls`, `AWS_SECRET_ACCESS_KEY=*** aws s3 ls`},
		{"passwd", "MYSQL_PASSWD=x1", "MYSQL_PASSWD=***"},
		{"auth", "NPM_AUTH=abc", "NPM_AUTH=***"},
		{"credential", "GOOGLE_CREDENTIALS=/k.json", "GOOGLE_CREDENTIALS=***"},
		{"session", "SESSION_ID=s1 next", "SESSION_ID=*** next"},
		{"cookie", "cookie=abc; other", "cookie=***; other"},
		{"camel case", "apiKey=zzz", "apiKey=***"},
		{"chained command", "TOKEN=abc&&deploy", "TOKEN=***&&deploy"},
		{"not a substring match: keyword", "KEYWORD=hello", "KEYWORD=hello"},
		{"not a substring match: monkey", "MONKEY=banana", "MONKEY=banana"},
		{"not a substring match: author", "AUTHOR=me", "AUTHOR=me"},
		{"plain assignment", "GOOS=linux go build", "GOOS=linux go build"},

		// Authorization headers and Bearer tokens.
		{"authorization header", `curl -H "Authorization: Bearer abc.def" https://x`, `curl -H "Authorization: ***" https://x`},
		{"authorization basic", "Authorization: Basic dXNlcjpwYXNz", "Authorization: ***"},
		{"authorization lower", "authorization:tok123", "authorization: ***"},
		{"bearer alone", "send Bearer eyJhbGciOi.x.y now", "send Bearer *** now"},

		// Flag arguments, both forms.
		{"password flag space", "mysql --password hunter2 -u root", "mysql --password *** -u root"},
		{"password flag equals", "mysql --password=hunter2 -u root", "mysql --password=*** -u root"},
		{"token flag", "gh auth login --token ghp_1", "gh auth login --token ***"},
		{"secret flag", "tool --secret=s1", "tool --secret=***"},
		{"api-key flag space", "cli --api-key k-123 run", "cli --api-key *** run"},
		{"api-key flag equals", `cli --api-key="k 1" run`, `cli --api-key=*** run`},
		{"flag followed by flag", "cli --token --verbose", "cli --token --verbose"},
		{"harmless flag", "ls --color always", "ls --color always"},

		// URL userinfo and query parameters.
		{"url userinfo", "git clone https://user:pa55@github.com/o/r.git", "git clone https://***@github.com/o/r.git"},
		{"url token only", "https://x-access-token:abc@host/p", "https://***@host/p"},
		{"url without userinfo", "https://github.com/o/r@v1", "https://github.com/o/r@v1"},
		{"query token", "curl 'https://api.x/v1?access_token=abc&page=2'", "curl 'https://api.x/v1?access_token=***&page=2'"},
		{"query key", "https://maps.x/api?key=AIza1&q=paris", "https://maps.x/api?key=***&q=paris"},
		{"query signature", "https://b.s3/o?X-Amz-Signature=deadbeef&x=1", "https://b.s3/o?X-Amz-Signature=***&x=1"},
		{"query harmless", "https://x/search?q=token", "https://x/search?q=token"},

		// GitHub tokens, wherever they appear.
		{"installation token", "token ghs_16C7e42F292c6912E7710c838347Ae178B4a failed", "token *** failed"},
		{"user token", "got ghu_abc123XYZ here", "got *** here"},
		{"oauth token", "gho_abc123XYZ", "***"},
		{"personal token", "clone https://github.com/o/r?x=ghp_abc123XYZ_def", "clone https://github.com/o/r?x=***"},
		{"refresh token", "ghr_abc123", "***"},
		{"fine-grained token", "use github_pat_11ABCDEFG0abc_DEFxyz999 now", "use *** now"},
		{"github authorization header", "Authorization: token ghs_abc123", "Authorization: ***"},
		{"github authorization quoted", `"Authorization: Bearer ghs_abc123"`, `"Authorization: ***"`},
		{"github token in header map", "map[Accept:[x] Authorization:[Bearer ghs_abc123]]", "map[Accept:[x] Authorization: ***]]"},
		{"token inside json", `{"token":"ghs_abc123","expires_at":"x"}`, `{"token":"***","expires_at":"x"}`},
		{"not a token prefix inside a word", "weighs_x ghsx_1 ghs_", "weighs_x ghsx_1 ghs_"},

		{"nothing", "echo hello world", "echo hello world"},
	} {
		if got := String(c.in); got != c.want {
			t.Errorf("%s: String(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestStringIdempotent(t *testing.T) {
	in := `TOKEN=a curl -H "Authorization: Bearer b" --password c https://u:p@h/?key=k`
	once := String(in)
	if twice := String(once); twice != once {
		t.Fatalf("String is not idempotent: %q then %q", once, twice)
	}
}
