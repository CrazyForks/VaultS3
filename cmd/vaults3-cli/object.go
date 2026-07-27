package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func runObject(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: vaults3-cli object <subcommand>

Subcommands:
  ls <bucket> [--prefix=<p>] [--recursive] [--max-keys=<n>]   List objects (folders + leaves; --recursive for all nested; paginates past 1000)
  put <bucket> <key> <file>                           Upload object
  get <bucket> <key> <file>                           Download object
  rm <bucket> <key>                                   Delete object
  cp <src-bucket/key> <dst-bucket/key>                Copy object
  presign <bucket> <key> [--expires=3600]             Generate presigned GET URL
  verify <bucket> [--prefix=<p>] [--repair]           Find objects that list but cannot be read (metadata/data desync); --repair removes the orphaned metadata`)
		os.Exit(1)
	}

	requireCreds()

	switch args[0] {
	case "ls", "list":
		objectList(args[1:])
	case "put", "upload":
		objectPut(args[1:])
	case "get", "download":
		objectGet(args[1:])
	case "rm", "delete":
		objectDelete(args[1:])
	case "cp", "copy":
		objectCopy(args[1:])
	case "presign":
		objectPresign(args[1:])
	case "verify", "fsck":
		objectVerify(args[1:])
	default:
		fatal("unknown object subcommand: " + args[0])
	}
}

func objectList(args []string) {
	if len(args) < 1 {
		fatal("object ls requires a bucket name")
	}
	bucket := args[0]
	prefix := ""
	recursive := false
	limit := 0 // 0 = list everything (paginating past the server's 1000-per-page cap)

	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--prefix="):
			prefix = strings.TrimPrefix(arg, "--prefix=")
		case arg == "--recursive" || arg == "-r":
			recursive = true
		case strings.HasPrefix(arg, "--max-keys="):
			if n, err := strconv.Atoi(strings.TrimPrefix(arg, "--max-keys=")); err == nil {
				limit = n
			}
		}
	}

	// Default behaviour matches `mc ls`: a "/" delimiter collapses each level into
	// folders (CommonPrefixes) and shows only immediate objects. --recursive drops
	// the delimiter for a full nested listing.
	delimiter := "/"
	if recursive {
		delimiter = ""
	}

	type contentT struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	}

	var objects []contentT
	var prefixes []string
	seenPrefix := map[string]bool{}
	token := ""

	for {
		pageSize := 1000
		if limit > 0 {
			remaining := limit - len(objects)
			if remaining <= 0 {
				break
			}
			if remaining < pageSize {
				pageSize = remaining
			}
		}

		path := fmt.Sprintf("/%s?list-type=2&max-keys=%d", bucket, pageSize)
		if prefix != "" {
			path += "&prefix=" + url.QueryEscape(prefix)
		}
		if delimiter != "" {
			path += "&delimiter=" + url.QueryEscape(delimiter)
		}
		if token != "" {
			path += "&continuation-token=" + url.QueryEscape(token)
		}

		resp, err := s3Request("GET", path, nil)
		if err != nil {
			fatal(err.Error())
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		}

		var result struct {
			XMLName        xml.Name   `xml:"ListBucketResult"`
			Contents       []contentT `xml:"Contents"`
			CommonPrefixes []struct {
				Prefix string `xml:"Prefix"`
			} `xml:"CommonPrefixes"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			fatal("parse response: " + err.Error())
		}
		resp.Body.Close()

		objects = append(objects, result.Contents...)
		for _, cp := range result.CommonPrefixes {
			if cp.Prefix != "" && !seenPrefix[cp.Prefix] {
				seenPrefix[cp.Prefix] = true
				prefixes = append(prefixes, cp.Prefix)
			}
		}

		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}

	if len(objects) == 0 && len(prefixes) == 0 {
		fmt.Println("No objects found.")
		return
	}

	headers := []string{"NAME", "SIZE", "LAST MODIFIED", "ETAG"}
	var rows [][]string
	for _, p := range prefixes { // folders first, like a file explorer
		rows = append(rows, []string{p, "DIR", "-", "-"})
	}
	for _, obj := range objects {
		t, _ := time.Parse(time.RFC3339Nano, obj.LastModified)
		rows = append(rows, []string{
			obj.Key,
			formatSize(obj.Size),
			t.Format("2006-01-02 15:04:05"),
			strings.Trim(obj.ETag, "\""),
		})
	}
	printTable(headers, rows)

	fmt.Printf("\n%d object(s)", len(objects))
	if len(prefixes) > 0 {
		fmt.Printf(", %d prefix(es)", len(prefixes))
	}
	fmt.Println()
}

func objectPut(args []string) {
	if len(args) < 3 {
		fatal("object put requires: <bucket> <key> <file>")
	}
	bucket, key, filePath := args[0], args[1], args[2]

	f, err := os.Open(filePath)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		fatal(err.Error())
	}

	data, err := io.ReadAll(f)
	if err != nil {
		fatal(err.Error())
	}

	path := fmt.Sprintf("/%s/%s", bucket, key)
	resp, err := s3Request("PUT", path, bytes.NewReader(data))
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		fmt.Printf("Uploaded '%s' to %s/%s (%s)\n", filePath, bucket, key, formatSize(stat.Size()))
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

func objectGet(args []string) {
	if len(args) < 3 {
		fatal("object get requires: <bucket> <key> <file>")
	}
	bucket, key, filePath := args[0], args[1], args[2]

	path := fmt.Sprintf("/%s/%s", bucket, key)
	resp, err := s3Request("GET", path, nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	out, err := os.Create(filePath)
	if err != nil {
		fatal(err.Error())
	}
	defer out.Close()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		fatal(err.Error())
	}

	fmt.Printf("Downloaded %s/%s to '%s' (%s)\n", bucket, key, filePath, formatSize(n))
}

func objectDelete(args []string) {
	if len(args) < 2 {
		fatal("object rm requires: <bucket> <key>")
	}
	bucket, key := args[0], args[1]

	path := fmt.Sprintf("/%s/%s", bucket, key)
	resp, err := s3Request("DELETE", path, nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 || resp.StatusCode == 200 {
		fmt.Printf("Deleted %s/%s\n", bucket, key)
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

func objectCopy(args []string) {
	if len(args) < 2 {
		fatal("object cp requires: <src-bucket/key> <dst-bucket/key>")
	}
	srcParts := strings.SplitN(args[0], "/", 2)
	dstParts := strings.SplitN(args[1], "/", 2)

	if len(srcParts) != 2 || len(dstParts) != 2 {
		fatal("source and destination must be in format: bucket/key")
	}

	path := fmt.Sprintf("/%s/%s", dstParts[0], dstParts[1])
	url := strings.TrimRight(endpoint, "/") + path

	req, err := newHTTPRequest("PUT", url, nil)
	if err != nil {
		fatal(err.Error())
	}
	req.Header.Set("X-Amz-Copy-Source", fmt.Sprintf("/%s/%s", srcParts[0], srcParts[1]))
	signV4(req, accessKey, secretKey, region)

	resp, err := httpClient().Do(req)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		fmt.Printf("Copied %s to %s\n", args[0], args[1])
	} else {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}
}

func objectPresign(args []string) {
	if len(args) < 2 {
		fatal("object presign requires: <bucket> <key> [--expires=3600]")
	}
	bucket, key := args[0], args[1]
	expires := 3600

	for _, arg := range args[2:] {
		if strings.HasPrefix(arg, "--expires=") {
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--expires="))
			if err == nil {
				expires = n
			}
		}
	}

	// Generate presigned URL locally
	now := time.Now().UTC()
	dateStr := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	u, _ := url.Parse(endpoint)
	host := u.Host

	credential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", accessKey, dateStr, region)

	params := url.Values{}
	params.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	params.Set("X-Amz-Credential", credential)
	params.Set("X-Amz-Date", amzDate)
	params.Set("X-Amz-Expires", strconv.Itoa(expires))
	params.Set("X-Amz-SignedHeaders", "host")

	canonicalURI := fmt.Sprintf("/%s/%s", bucket, key)
	canonicalQueryString := params.Encode()
	canonicalHeaders := fmt.Sprintf("host:%s\n", host)
	signedHeaders := "host"

	canonicalRequest := fmt.Sprintf("GET\n%s\n%s\n%s\n%s\nUNSIGNED-PAYLOAD",
		canonicalURI, canonicalQueryString, canonicalHeaders, signedHeaders)

	hash := sha256.Sum256([]byte(canonicalRequest))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, hex.EncodeToString(hash[:]))

	kDate := hmacSign([]byte("AWS4"+secretKey), []byte(dateStr))
	kRegion := hmacSign(kDate, []byte(region))
	kService := hmacSign(kRegion, []byte("s3"))
	kSigning := hmacSign(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSign(kSigning, []byte(stringToSign)))

	params.Set("X-Amz-Signature", signature)

	fmt.Printf("%s%s?%s\n", endpoint, canonicalURI, params.Encode())
}

// objectVerify walks every object in a bucket and probes whether its data is
// actually readable. It catches the metadata/data desync where an object lists
// fine (its metadata exists) but a GET returns "Object not found" because the
// underlying data is missing (issue #40). With --repair it removes the orphaned
// metadata so the phantom stops appearing in listings. It never deletes readable
// data: repair only touches keys that already fail to read.
func objectVerify(args []string) {
	if len(args) < 1 {
		fatal("object verify requires a bucket name")
	}
	bucket := args[0]
	prefix := ""
	repair := false
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--prefix="):
			prefix = strings.TrimPrefix(arg, "--prefix=")
		case arg == "--repair":
			repair = true
		}
	}

	keys := listAllObjectKeys(bucket, prefix)
	fmt.Printf("Verifying %d object(s) in bucket %q...\n", len(keys), bucket)

	var orphans []string
	for _, key := range keys {
		// Directory markers (keys ending in "/") have no readable body by design;
		// skip them so they are not reported as orphans.
		if strings.HasSuffix(key, "/") {
			continue
		}
		status, err := probeObjectReadable(bucket, key)
		if err != nil {
			fatal(err.Error())
		}
		// 404 means metadata exists (it is in the listing) but the data cannot be
		// read: a genuine desync. Any other status (200/206/416 for zero-byte) is fine.
		if status == http.StatusNotFound {
			orphans = append(orphans, key)
		}
	}

	if len(orphans) == 0 {
		fmt.Println("OK: every listed object is readable, no metadata/data desync found.")
		return
	}

	fmt.Printf("\nFound %d object(s) that list but cannot be read (data missing):\n", len(orphans))
	for _, key := range orphans {
		fmt.Printf("  %s\n", key)
	}

	if !repair {
		fmt.Println("\nRe-run with --repair to remove the orphaned metadata for these keys.")
		return
	}

	fmt.Println("\nRepairing (removing orphaned metadata)...")
	repaired := 0
	for _, key := range orphans {
		u := objectURL(bucket, key)
		req, err := http.NewRequest("DELETE", u, nil)
		if err != nil {
			fmt.Printf("  FAILED %s: %v\n", key, err)
			continue
		}
		signV4(req, accessKey, secretKey, region)
		resp, err := httpClient().Do(req)
		if err != nil {
			fmt.Printf("  FAILED %s: %v\n", key, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 204 || resp.StatusCode == 200 {
			fmt.Printf("  removed %s\n", key)
			repaired++
		} else {
			fmt.Printf("  FAILED %s: HTTP %d\n", key, resp.StatusCode)
		}
	}
	fmt.Printf("\nRepaired %d of %d orphan(s).\n", repaired, len(orphans))
}

// listAllObjectKeys returns every object key in a bucket (recursive, no delimiter),
// paginating past the server's per-page cap.
func listAllObjectKeys(bucket, prefix string) []string {
	var keys []string
	token := ""
	for {
		path := fmt.Sprintf("/%s?list-type=2&max-keys=1000", bucket)
		if prefix != "" {
			path += "&prefix=" + url.QueryEscape(prefix)
		}
		if token != "" {
			path += "&continuation-token=" + url.QueryEscape(token)
		}
		resp, err := s3Request("GET", path, nil)
		if err != nil {
			fatal(err.Error())
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		}
		var result struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			fatal("parse response: " + err.Error())
		}
		resp.Body.Close()
		for _, c := range result.Contents {
			keys = append(keys, c.Key)
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	return keys
}

// probeObjectReadable issues a 1-byte ranged GET so a healthy object is not fully
// downloaded, and returns the HTTP status. A 404 means the data is unreadable.
func probeObjectReadable(bucket, key string) (int, error) {
	req, err := http.NewRequest("GET", objectURL(bucket, key), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	signV4(req, accessKey, secretKey, region)
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// objectURL builds a properly path-encoded S3 URL for a bucket/key, so keys with
// spaces or other special characters sign and route correctly.
func objectURL(bucket, key string) string {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return strings.TrimRight(endpoint, "/") + "/" + bucket + "/" + key
	}
	u.Path = "/" + bucket + "/" + key
	return u.String()
}

func newHTTPRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}
