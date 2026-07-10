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
  presign <bucket> <key> [--expires=3600]             Generate presigned GET URL`)
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

func newHTTPRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}
