package metadata

import (
	"path/filepath"
	"testing"
)

func newPolicyStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBucket("photos"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	return s
}

func setPolicy(t *testing.T, s *Store, policy string) {
	t.Helper()
	if err := s.PutBucketPolicy("photos", []byte(policy)); err != nil {
		t.Fatalf("PutBucketPolicy: %v", err)
	}
}

// TestPublicReadPrincipalFormats covers issue #41: the AWS standard object form
// of Principal ({"AWS": "*"} and {"AWS": ["*"]}) must be recognised as public,
// not just the shorthand string form.
func TestPublicReadPrincipalFormats(t *testing.T) {
	cases := []struct {
		name      string
		principal string
		want      bool
	}{
		{"string wildcard", `"*"`, true},
		{"aws object wildcard", `{"AWS": "*"}`, true},
		{"aws object wildcard list", `{"AWS": ["*"]}`, true},
		{"aws object list with wildcard among others", `{"AWS": ["arn:aws:iam::123456789012:root", "*"]}`, true},
		{"lowercase aws key", `{"aws": "*"}`, true},
		// Must NOT be treated as public — these name specific principals.
		{"specific account", `{"AWS": "arn:aws:iam::123456789012:root"}`, false},
		{"specific user list", `{"AWS": ["arn:aws:iam::123456789012:user/bob"]}`, false},
		{"service principal", `{"Service": "cloudtrail.amazonaws.com"}`, false},
		{"canonical user", `{"CanonicalUser": "abc123"}`, false},
		{"empty", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Version": "2012-10-17",
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": `+tc.principal+`,
			    "Action": ["s3:GetObject"],
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if got := s.IsBucketPublicRead("photos"); got != tc.want {
				t.Fatalf("IsBucketPublicRead = %v, want %v (principal %s)", got, tc.want, tc.principal)
			}
		})
	}
}

// TestPublicReadActionForms checks action matching, including wildcards and the
// single-string form.
func TestPublicReadActionForms(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{`"s3:GetObject"`, true},
		{`["s3:GetObject"]`, true},
		{`"s3:*"`, true},
		{`"s3:Get*"`, true},
		{`["s3:PutObject", "s3:GetObject"]`, true},
		{`"s3:PutObject"`, false},
		{`["s3:ListBucket"]`, false}, // listing is not object read
		{`"*"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": `+tc.action+`,
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if got := s.IsBucketPublicRead("photos"); got != tc.want {
				t.Fatalf("action %s: got %v want %v", tc.action, got, tc.want)
			}
		})
	}
}

// TestPublicListIsSeparateFromRead is the security boundary for the "also support
// s3:ListBucket" request on issue #41: granting only s3:ListBucket must make the
// listing public WITHOUT making every object readable, and vice versa.
func TestPublicListIsSeparateFromRead(t *testing.T) {
	t.Run("list only", func(t *testing.T) {
		s := newPolicyStore(t)
		setPolicy(t, s, `{
		  "Statement": [{
		    "Effect": "Allow",
		    "Principal": {"AWS": "*"},
		    "Action": ["s3:ListBucket"],
		    "Resource": ["arn:aws:s3:::photos"]
		  }]
		}`)
		if !s.IsBucketPublicList("photos") {
			t.Fatal("s3:ListBucket should make the bucket publicly listable")
		}
		if s.IsBucketPublicRead("photos") {
			t.Fatal("SECURITY: s3:ListBucket must NOT make objects publicly readable")
		}
	})

	t.Run("read only", func(t *testing.T) {
		s := newPolicyStore(t)
		setPolicy(t, s, `{
		  "Statement": [{
		    "Effect": "Allow",
		    "Principal": {"AWS": "*"},
		    "Action": ["s3:GetObject"],
		    "Resource": ["arn:aws:s3:::photos/*"]
		  }]
		}`)
		if !s.IsBucketPublicRead("photos") {
			t.Fatal("s3:GetObject should make objects publicly readable")
		}
		if s.IsBucketPublicList("photos") {
			t.Fatal("SECURITY: s3:GetObject must NOT make the bucket publicly listable")
		}
	})
}

// TestPublicReadExplicitDenyWins verifies AWS evaluation order: an explicit Deny
// overrides an Allow, so a bucket with both is not public.
func TestPublicReadExplicitDenyWins(t *testing.T) {
	s := newPolicyStore(t)
	setPolicy(t, s, `{
	  "Statement": [
	    {"Effect": "Allow", "Principal": {"AWS": "*"}, "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::photos/*"]},
	    {"Effect": "Deny",  "Principal": {"AWS": "*"}, "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::photos/*"]}
	  ]
	}`)
	if s.IsBucketPublicRead("photos") {
		t.Fatal("SECURITY: an explicit Deny must override the Allow")
	}
}

// TestPublicReadResourceMustMatchBucket stops a policy written for another bucket
// from making this one public.
func TestPublicReadResourceMustMatchBucket(t *testing.T) {
	cases := []struct {
		resource string
		want     bool
	}{
		{`["arn:aws:s3:::photos/*"]`, true},
		{`["arn:aws:s3:::photos"]`, true},
		{`"arn:aws:s3:::photos/public/*"`, true},
		{`["*"]`, true},
		{`["arn:aws:s3:::*"]`, true},
		{`["arn:aws:s3:::other-bucket/*"]`, false},
		{`["arn:aws:s3:::photos-archive/*"]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.resource, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": ["s3:GetObject"],
			    "Resource": `+tc.resource+`
			  }]
			}`)
			if got := s.IsBucketPublicRead("photos"); got != tc.want {
				t.Fatalf("resource %s: got %v want %v", tc.resource, got, tc.want)
			}
		})
	}
}

// TestPublicAccessBlockOverridesPolicy verifies the Public Access Block setting is
// actually enforced: it was previously stored and reported but never consulted, so
// enabling it did not stop anonymous access.
func TestPublicAccessBlockOverridesPolicy(t *testing.T) {
	for _, field := range []string{"BlockPublicPolicy", "RestrictPublicBuckets"} {
		t.Run(field, func(t *testing.T) {
			s := newPolicyStore(t)
			setPolicy(t, s, `{
			  "Statement": [{
			    "Effect": "Allow",
			    "Principal": {"AWS": "*"},
			    "Action": ["s3:GetObject"],
			    "Resource": ["arn:aws:s3:::photos/*"]
			  }]
			}`)
			if !s.IsBucketPublicRead("photos") {
				t.Fatal("precondition: bucket should be public before the block")
			}

			cfg := PublicAccessBlockConfig{}
			if field == "BlockPublicPolicy" {
				cfg.BlockPublicPolicy = true
			} else {
				cfg.RestrictPublicBuckets = true
			}
			if err := s.PutPublicAccessBlock("photos", cfg); err != nil {
				t.Fatalf("PutPublicAccessBlock: %v", err)
			}
			if s.IsBucketPublicRead("photos") {
				t.Fatalf("SECURITY: %s must block anonymous read access", field)
			}
		})
	}
}

// TestNoPolicyIsNotPublic is the default-deny guard.
func TestNoPolicyIsNotPublic(t *testing.T) {
	s := newPolicyStore(t)
	if s.IsBucketPublicRead("photos") || s.IsBucketPublicList("photos") {
		t.Fatal("a bucket with no policy must not be public")
	}
	setPolicy(t, s, `{not valid json`)
	if s.IsBucketPublicRead("photos") {
		t.Fatal("a malformed policy must not be treated as public")
	}
}
