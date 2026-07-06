package s3

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// requestBody returns the logical object bytes of an upload request. Recent
// AWS SDKs default to streaming uploads with trailing checksums, which wrap
// the payload in aws-chunked encoding; we decode the framing and ignore
// chunk signatures and trailers (checksums are verified end-to-end by
// clients on download instead).
func requestBody(r *http.Request) io.Reader {
	contentSHA := r.Header.Get("x-amz-content-sha256")
	if strings.HasPrefix(contentSHA, "STREAMING-") ||
		strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return &awsChunkedReader{br: bufio.NewReader(r.Body)}
	}
	return r.Body
}

// awsChunkedReader decodes aws-chunked framing:
//
//	hex-size[;chunk-signature=...]\r\n <bytes> \r\n ... 0[;...]\r\n [trailers] \r\n
type awsChunkedReader struct {
	br        *bufio.Reader
	remaining int64
	done      bool
}

func (c *awsChunkedReader) Read(p []byte) (int, error) {
	for {
		if c.done {
			return 0, io.EOF
		}
		if c.remaining > 0 {
			if int64(len(p)) > c.remaining {
				p = p[:c.remaining]
			}
			n, err := c.br.Read(p)
			c.remaining -= int64(n)
			if c.remaining == 0 {
				if err2 := c.discardCRLF(); err == nil {
					err = err2
				}
			}
			return n, err
		}
		header, err := c.br.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("reading aws-chunked header: %w", err)
		}
		header = strings.TrimRight(header, "\r\n")
		if idx := strings.IndexByte(header, ';'); idx >= 0 {
			header = header[:idx] // drop chunk-signature extension
		}
		size, err := strconv.ParseInt(strings.TrimSpace(header), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid aws-chunked size %q: %w", header, err)
		}
		if size == 0 {
			// Consume (and ignore) trailers up to the final blank line
			// or EOF.
			for {
				line, err := c.br.ReadString('\n')
				if err == io.EOF || strings.TrimRight(line, "\r\n") == "" {
					break
				}
				if err != nil {
					return 0, err
				}
			}
			c.done = true
			return 0, io.EOF
		}
		c.remaining = size
	}
}

func (c *awsChunkedReader) discardCRLF() error {
	for i := 0; i < 2; i++ {
		b, err := c.br.ReadByte()
		if err != nil {
			return err
		}
		if b != '\r' && b != '\n' {
			return fmt.Errorf("expected CRLF after aws-chunked chunk, got %q", b)
		}
	}
	return nil
}
