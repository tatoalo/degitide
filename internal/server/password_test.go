package server

import "testing"

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !verifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password did not verify")
	}
	if verifyPassword(encoded, "wrong password") {
		t.Fatal("wrong password verified")
	}
}

func TestPasswordHashRejectsMalformedOrExcessiveParameters(t *testing.T) {
	cases := []string{
		"",
		"$argon2id$v=19$m=999999,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for _, encoded := range cases {
		if verifyPassword(encoded, "password") {
			t.Fatalf("malformed password hash verified: %q", encoded)
		}
	}
}
