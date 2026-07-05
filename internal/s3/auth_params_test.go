package s3

import "testing"

// Regression for issue #22: the SigV4 Authorization header separates its
// components with commas and OPTIONAL whitespace. VaultS3 previously split on
// ", " (comma-space) only, so clients that omit the space (WinSCP, S3 Browser,
// and others) parsed to empty SignedHeaders/Signature and were rejected with
// "missing auth parameters" (a 403 logged as Deny). Both forms must parse.
func TestParseAuthParamsCommaVariants(t *testing.T) {
	want := map[string]string{
		"Credential":    "AKID/20260705/us-east-1/s3/aws4_request",
		"SignedHeaders": "host;x-amz-content-sha256;x-amz-date",
		"Signature":     "abc123def456",
	}

	cases := map[string]string{
		"space after comma (boto3/aws-cli)": "Credential=AKID/20260705/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abc123def456",
		"no space after comma (WinSCP/S3B)": "Credential=AKID/20260705/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=abc123def456",
		"mixed whitespace":                  "Credential=AKID/20260705/us-east-1/s3/aws4_request,  SignedHeaders=host;x-amz-content-sha256;x-amz-date ,Signature=abc123def456",
	}

	for name, header := range cases {
		got := parseAuthParams(header)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
}
