package builtin

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"llama-webui/server/internal/contracts"
)

// readFileChunk is how much the line scanner pulls off the disk between two
// looks at the request context.
const readFileChunk = 64 * 1024

// readCapped reads a stream but keeps at most limit+1 bytes, so a file whose
// reported size is wrong cannot make the caller allocate without bound. over
// says the extra byte showed up, which means the real size is past the limit.
func readCapped(r io.Reader, limit int64) (content []byte, over bool, err error) {
	content, err = io.ReadAll(io.LimitReader(r, limit+1))
	return content, int64(len(content)) > limit, err
}

func readFile(ctx context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}
	startLine := optInt(params, "start_line", 1)
	endLine := optInt(params, "end_line", -1)
	appendLoc := optBool(params, "append_loc", false)
	asBase64 := req.RespType == "base64"

	full := resolvePath(s.dir(req), path)

	info, err := os.Stat(full)
	if err != nil {
		return fail("cannot stat file: " + path)
	}
	// Both limits below are sizes, and only a regular file has one that means
	// something. A character device or a fifo reports 0 bytes, so /dev/zero
	// passed the gate and the read grew until the runtime aborted the process.
	// That abort is fatal: it is not a panic, so toolsapi cannot recover it.
	if !info.Mode().IsRegular() {
		return fail("cannot read file: " + path)
	}

	if asBase64 {
		if info.Size() > readFileMaxSizeB64 {
			return fail(fmt.Sprintf("file too large (%d bytes, max %d)", info.Size(), readFileMaxSizeB64))
		}
		f, err := os.Open(full)
		if err != nil {
			return fail("failed to open file: " + path)
		}
		// The read is bounded as well: one byte past the cap is enough to show
		// the size was wrong, and it stops a lying file from being drained.
		content, over, err := readCapped(f, readFileMaxSizeB64)
		f.Close()
		if err != nil {
			return fail("failed to open file: " + path)
		}
		if over {
			return fail(fmt.Sprintf("file too large (%d bytes, max %d)", len(content), readFileMaxSizeB64))
		}
		return contracts.Result{Body: map[string]any{
			"base64":     base64.StdEncoding.EncodeToString(content),
			"size_bytes": len(content),
		}}
	}

	if info.Size() > readFileMaxSize && endLine == -1 {
		return fail(fmt.Sprintf("file too large (%d bytes, max %d). Use start_line/end_line to read a portion.",
			info.Size(), readFileMaxSize))
	}

	// A range skips the size gate above, so the file is streamed instead of
	// loaded: llama-server reads it whole and pays for every byte that comes
	// before start_line.
	f, err := os.Open(full)
	if err != nil {
		return fail("failed to open file: " + path)
	}
	defer f.Close()

	text, err := scanFile(ctx, f, startLine, endLine, appendLoc)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr.Error())
		}
		return fail("failed to open file: " + path)
	}
	return contracts.Result{PlainText: text}
}

// lineEnd says how one line read finished.
type lineEnd int

const (
	lineNone lineEnd = iota // nothing was left: the file ended on a newline
	lineDone                // the line ended with its '\n'
	lineLast                // the file ended mid-line, so this was the last one
	lineOver                // the line outgrew the room left in the output
)

// scanFile builds the answer for a text read. The output is what splitLines
// over the whole content would have produced, cut to [start_line, end_line] and
// capped at readFileMaxSize bytes, but no more of the file than that needs is
// read or held: the walk stops at end_line, and a line too long for the room
// left only has to prove that it is too long.
func scanFile(ctx context.Context, f *os.File, startLine, endLine int, appendLoc bool) (string, error) {
	r := bufio.NewReaderSize(f, readFileChunk)
	var sb strings.Builder

	for lineno := 1; endLine == -1 || lineno <= endLine; lineno++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Before the range: a line is worth its newline, so it is dropped whole.
		if lineno < startLine {
			more, err := skipLine(ctx, r)
			if err != nil {
				return "", err
			}
			if !more {
				break
			}
			continue
		}

		room := readFileMaxSize - sb.Len() - 1
		if appendLoc {
			room -= len(strconv.Itoa(lineno)) + len("→")
		}
		line, how, err := readLine(r, room)
		if err != nil {
			return "", err
		}
		if how == lineNone {
			break
		}
		if how == lineOver {
			sb.WriteString(truncatedMarker)
			break
		}
		out := string(line) + "\n"
		if appendLoc {
			out = fmt.Sprintf("%d→%s\n", lineno, line)
		}
		sb.WriteString(out)
		if how == lineLast {
			break
		}
	}
	return sb.String(), nil
}

// skipLine throws away the line the reader is inside of. It reports whether a
// line was there at all: at the end of the file there is none left to skip.
func skipLine(ctx context.Context, r *bufio.Reader) (bool, error) {
	sawByte := false
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// ReadSlice hands back everything up to the next newline, or one full
		// buffer when a single line is longer than that.
		part, err := r.ReadSlice('\n')
		if len(part) > 0 {
			sawByte = true
		}
		if err == nil {
			return true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return sawByte, nil
		}
		return false, err
	}
}

// readLine reads the line the reader is inside of, keeping at most room bytes.
// A line that runs past the room cannot reach the output whatever follows it,
// so its tail is not read: the caller answers with the truncation marker.
func readLine(r *bufio.Reader, room int) ([]byte, lineEnd, error) {
	if room < 0 {
		// Even an empty line would not fit here. The old code still waited for a
		// line to exist before it wrote the marker, so look for its first byte.
		_, err := r.Peek(1)
		if errors.Is(err, io.EOF) {
			return nil, lineNone, nil
		}
		if err != nil {
			return nil, lineNone, err
		}
		return nil, lineOver, nil
	}

	line := make([]byte, 0, 256)
	for len(line) <= room {
		c, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, lineNone, nil
			}
			return line, lineLast, nil
		}
		if err != nil {
			return nil, lineNone, err
		}
		if c == '\n' {
			return line, lineDone, nil
		}
		line = append(line, c)
	}
	return line, lineOver, nil
}
