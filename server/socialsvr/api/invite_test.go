package api_test

import (
	"strings"
	"testing"

	"farm/server/shared/errcode"
	socialapi "farm/server/socialsvr/api"
)

const testSecret = "test-invite-secret-do-not-use-in-prod"

func TestIssueAndParseInvite(t *testing.T) {
	const inviter uint64 = 42
	const now int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, now, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if !strings.Contains(token, ".") {
		t.Fatalf("token %q missing payload.sig separator", token)
	}

	got, code := socialapi.ParseInvite(token, []byte(testSecret), now)
	if code != errcode.OK {
		t.Fatalf("ParseInvite code = %d (%s), want OK", code, code)
	}
	if got != inviter {
		t.Fatalf("ParseInvite uid = %d, want %d", got, inviter)
	}
}

func TestParseInviteTamperedSignature(t *testing.T) {
	const inviter uint64 = 7
	const now int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, now, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	// 翻转 sig 末位一个字符：base64url 字母表内替换，保证仍为合法 base64url 但与原文不同。
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		t.Fatal("missing dot")
	}
	sig := token[dot+1:]
	if len(sig) < 2 {
		t.Fatal("sig too short")
	}
	flipped := sig[len(sig)-1]
	switch flipped {
	case 'A':
		sig = sig[:len(sig)-1] + "B"
	case 'B':
		sig = sig[:len(sig)-1] + "A"
	default:
		sig = sig[:len(sig)-1] + "A"
	}
	tampered := token[:dot+1] + sig

	if _, code := socialapi.ParseInvite(tampered, []byte(testSecret), now); code != errcode.InviteInvalid {
		t.Fatalf("tampered sig: code = %d, want InviteInvalid(1406)", code)
	}
}

func TestParseInviteTamperedPayload(t *testing.T) {
	const inviter uint64 = 9
	const now int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, now, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	dot := strings.LastIndex(token, ".")
	payload := token[:dot]
	if len(payload) < 2 {
		t.Fatal("payload too short")
	}
	flipped := payload[len(payload)-1]
	switch flipped {
	case 'A':
		payload = payload[:len(payload)-1] + "B"
	case 'B':
		payload = payload[:len(payload)-1] + "A"
	default:
		payload = payload[:len(payload)-1] + "A"
	}
	tampered := payload + token[dot:]

	if _, code := socialapi.ParseInvite(tampered, []byte(testSecret), now); code != errcode.InviteInvalid {
		t.Fatalf("tampered payload: code = %d, want InviteInvalid(1406)", code)
	}
}

func TestParseInviteWrongSecret(t *testing.T) {
	const inviter uint64 = 11
	const now int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, now, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	if _, code := socialapi.ParseInvite(token, []byte("different-secret"), now); code != errcode.InviteInvalid {
		t.Fatalf("wrong secret: code = %d, want InviteInvalid(1406)", code)
	}
}

func TestParseInviteExpired(t *testing.T) {
	const inviter uint64 = 13
	const issueTime int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, issueTime, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	// 7 天 + 1 毫秒后必过期。
	const afterExpiry = issueTime + 7*24*60*60*1000 + 1
	if _, code := socialapi.ParseInvite(token, []byte(testSecret), afterExpiry); code != errcode.InviteExpired {
		t.Fatalf("expired: code = %d, want InviteExpired(1407)", code)
	}
}

func TestParseInviteJustBeforeExpiry(t *testing.T) {
	const inviter uint64 = 15
	const issueTime int64 = 1_700_000_000_000

	token, err := socialapi.IssueInvite(inviter, issueTime, []byte(testSecret))
	if err != nil {
		t.Fatalf("IssueInvite: %v", err)
	}

	const atExpiry = issueTime + 7*24*60*60*1000
	got, code := socialapi.ParseInvite(token, []byte(testSecret), atExpiry)
	if code != errcode.OK {
		t.Fatalf("at expiry: code = %d, want OK", code)
	}
	if got != inviter {
		t.Fatalf("uid = %d, want %d", got, inviter)
	}
}

func TestParseInviteMalformed(t *testing.T) {
	cases := []string{
		"",
		"no-dot",
		"not-base64!.abc",
		"abc.",
		".abc",
	}
	for _, tok := range cases {
		if _, code := socialapi.ParseInvite(tok, []byte(testSecret), 1); code != errcode.InviteInvalid {
			t.Errorf("ParseInvite(%q) code = %d, want InviteInvalid(1406)", tok, code)
		}
	}
}

func TestIssueInviteRejectsEmptySecret(t *testing.T) {
	if _, err := socialapi.IssueInvite(1, 1, nil); err == nil {
		t.Fatal("IssueInvite with nil secret should error")
	}
	if _, err := socialapi.IssueInvite(1, 1, []byte{}); err == nil {
		t.Fatal("IssueInvite with empty secret should error")
	}
}
