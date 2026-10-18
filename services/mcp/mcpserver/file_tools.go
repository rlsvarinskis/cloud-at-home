package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
)

const (
	// filMaxFileBytes is the vmd limit for PutFile content and for GetFile when base64 output is requested.
	filMaxFileBytes = 8 << 20
	// filMaxTextBytes caps text downloads (and is the default for every download) so tool output stays small.
	filMaxTextBytes = 1 << 20
	// filDefaultFetchWait is how long agent_fetch waits for the download when wait_seconds is not given.
	filDefaultFetchWait = 45
	// filSSHKeyWait is how long agent_ssh_pubkey waits for the key to be read or generated.
	filSSHKeyWait = 20
	// filFetchTimeout is the job lifetime (vmd kills the download after this) in seconds.
	filFetchTimeout = 3600
	// filStderrTail is how many bytes of stderr an error reports.
	filStderrTail = 1000
	// filSSHKeyCommand prints the machine's ed25519 public key, creating the key pair first when there is none.
	filSSHKeyCommand = "cat ~/.ssh/id_ed25519.pub 2>/dev/null || (mkdir -p ~/.ssh && ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519 && cat ~/.ssh/id_ed25519.pub)"
)

type filUploadArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine (VM) to write the file to"`
	Path    string `json:"path" jsonschema:"absolute path inside the machine, for example /home/agent/data.csv"`
	Content string `json:"content" jsonschema:"file content: plain text, or base64 text when base64 is true. At most 8 MiB after decoding"`
	Base64  bool   `json:"base64,omitempty" jsonschema:"true when content is base64 of the file bytes (use for binary files); default false"`
	Mode    string `json:"mode,omitempty" jsonschema:"octal permission bits such as 0644 or 0755; default 0644"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type filDownloadArgs struct {
	Name     string `json:"name" jsonschema:"name of the machine (VM) to read the file from"`
	Path     string `json:"path" jsonschema:"path of the file inside the machine"`
	Encoding string `json:"encoding,omitempty" jsonschema:"auto (default: UTF-8 text if the file is valid UTF-8, else base64), utf-8 (fails on binary files) or base64 (the raw bytes as base64)"`
	Base64   bool   `json:"base64,omitempty" jsonschema:"deprecated alias for encoding=base64"`
	MaxBytes *int   `json:"max_bytes,omitempty" jsonschema:"most bytes to read; default 1048576 (1 MiB). utf-8 is capped at 1 MiB; auto and base64 may go up to 8 MiB"`
	Project  string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

// filDownloadResult is what agent_download returns, as JSON. size is the number of bytes returned, which is the
// file size unless truncated is set (vmd does not report the size of a larger file). sha256 is over those raw bytes.
type filDownloadResult struct {
	Path      string `json:"path"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

type filFetchArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine (VM) that downloads the file"`
	URL         string `json:"url" jsonschema:"http:// or https:// URL to download. The machine downloads it itself, so the bytes never pass through this server"`
	Path        string `json:"path" jsonschema:"where to save the file inside the machine: an absolute path, or relative to the agent user's home. Missing directories are created"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"how long this call may wait for the download, in seconds; default 45. Capped by the server's maximum wait. If the download is still running, poll it with agent_exec_poll"`
	Project     string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type filSSHPubkeyArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine (VM) whose SSH public key is wanted"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

func (s *Server) registerFileTools() {
	addTool(s, "agent_upload",
		"Write a file into a machine. content is plain text by default; set base64=true when content is base64 of binary bytes. "+
			"path must be absolute. mode is octal permission bits (default 0644). The file is limited to 8 MiB after decoding.",
		s.agentUpload)
	addTool(s, "agent_download",
		"Read a file from a machine. The reply is JSON: path, size (bytes returned), sha256 (of the raw bytes), encoding "+
			"and content. encoding auto (default) gives UTF-8 text when the file is valid UTF-8 and base64 otherwise; "+
			"utf-8 fails on binary files; base64 always gives base64. max_bytes limits the read (default 1 MiB; utf-8 is "+
			"capped at 1 MiB, auto and base64 may go up to 8 MiB). A truncated read sets truncated=true.",
		s.agentDownload)
	addTool(s, "agent_fetch",
		"Download a http:// or https:// URL inside a machine (the machine fetches it, so large files do not pass through this server) "+
			"and save it to path, creating directories as needed. Waits up to wait_seconds (default 45, capped by the server). "+
			"If it is still running, the reply gives a job ID to poll with agent_exec_poll.",
		s.agentFetch)
	addTool(s, "agent_ssh_pubkey",
		"Return the SSH public key (ed25519) of a machine, generating a key pair there if it has none. "+
			"Add the key to GitHub or to authorized_keys. The private key never leaves the machine.",
		s.agentSSHPubkey)
}

// agentUpload writes a file into the VM. All validation happens before any downstream call.
func (s *Server) agentUpload(ctx context.Context, in filUploadArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine to write to")
	}
	if !strings.HasPrefix(in.Path, "/") {
		return "", fmt.Errorf("path must be an absolute path inside the machine, for example /home/agent/file.txt (got %q)", in.Path)
	}
	if strings.ContainsRune(in.Path, 0) {
		return "", errors.New("path must not contain NUL bytes")
	}
	mode, err := filParseMode(in.Mode)
	if err != nil {
		return "", err
	}
	data, err := filDecodeContent(in.Content, in.Base64)
	if err != nil {
		return "", err
	}
	if len(data) > filMaxFileBytes {
		return "", fmt.Errorf("file is %d bytes after decoding; uploads are limited to %d bytes (8 MiB)", len(data), filMaxFileBytes)
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	if _, err := s.cfg.Clients.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{
		VmId: vm.GetId(), Path: in.Path, Content: data, Mode: mode})); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(data), in.Path), nil
}

// agentDownload reads a file from the VM and returns it as a JSON object (see filDownloadResult).
func (s *Server) agentDownload(ctx context.Context, in filDownloadArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine to read from")
	}
	if in.Path == "" {
		return "", errors.New("path is required: the file to read inside the machine")
	}
	if strings.ContainsRune(in.Path, 0) {
		return "", errors.New("path must not contain NUL bytes")
	}
	enc := strings.ToLower(strings.TrimSpace(in.Encoding))
	switch {
	case enc == "" && in.Base64:
		enc = "base64"
	case enc == "":
		enc = "auto"
	case enc != "auto" && enc != "utf-8" && enc != "base64":
		return "", fmt.Errorf("encoding must be auto, utf-8 or base64 (got %q)", in.Encoding)
	}
	limit := filMaxFileBytes // auto and base64 may read up to 8 MiB
	if enc == "utf-8" {
		limit = filMaxTextBytes
	}
	maxBytes := filMaxTextBytes
	if in.MaxBytes != nil {
		if *in.MaxBytes < 1 || *in.MaxBytes > limit {
			return "", fmt.Errorf("max_bytes must be between 1 and %d for this download", limit)
		}
		maxBytes = *in.MaxBytes
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	res, err := s.cfg.Clients.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{
		VmId: vm.GetId(), Path: in.Path, MaxBytes: int64(maxBytes)}))
	if err != nil {
		return "", err
	}
	data := res.Msg.GetContent()
	truncated := res.Msg.GetTruncated()
	if truncated && enc != "base64" {
		// The cut may fall inside a multi-byte character; drop that incomplete tail before the UTF-8 check.
		data = filTrimPartialUTF8(data)
	}
	isText := utf8.Valid(data)
	if enc == "utf-8" && !isText {
		return "", fmt.Errorf("%s is not UTF-8 text; call again with encoding=base64 to get its bytes as base64", in.Path)
	}
	out := filDownloadResult{Path: in.Path, Size: len(data), Truncated: truncated}
	sum := sha256.Sum256(data)
	out.SHA256 = hex.EncodeToString(sum[:])
	if enc == "base64" || (enc == "auto" && !isText) {
		out.Encoding, out.Content = "base64", base64.StdEncoding.EncodeToString(data)
	} else {
		out.Encoding, out.Content = "utf-8", string(data)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode download result: %w", err)
	}
	return string(raw), nil
}

// agentFetch makes the VM download a URL with curl, then reports the saved file.
func (s *Server) agentFetch(ctx context.Context, in filFetchArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine that downloads the file")
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be an absolute http:// or https:// URL (got %q)", in.URL)
	}
	if in.Path == "" {
		return "", errors.New("path is required: where to save the file inside the machine")
	}
	if strings.ContainsRune(in.Path, 0) {
		return "", errors.New("path must not contain NUL bytes")
	}
	wait := filDefaultFetchWait
	if in.WaitSeconds != nil {
		wait = *in.WaitSeconds
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	job, done, err := s.filRunJob(ctx, vm.GetId(), filFetchCommand(in.Path, in.URL), wait)
	if err != nil {
		return "", err
	}
	id := job.GetJob().GetId()
	if !done {
		return fmt.Sprintf("download still running as job %s: poll it with agent_exec_poll", id), nil
	}
	if code := job.GetJob().ExitCode; code == nil || *code != 0 {
		return "", filFailure("download", job)
	}
	return "ok: " + strings.TrimSpace(string(job.GetStdout())), nil
}

// agentSSHPubkey returns the VM's ed25519 public key, creating the key pair on first use.
func (s *Server) agentSSHPubkey(ctx context.Context, in filSSHPubkeyArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine whose key is wanted")
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	job, done, err := s.filRunJob(ctx, vm.GetId(), filSSHKeyCommand, filSSHKeyWait)
	if err != nil {
		return "", err
	}
	if !done {
		return fmt.Sprintf("ssh key setup still running as job %s: poll it with agent_exec_poll", job.GetJob().GetId()), nil
	}
	if code := job.GetJob().ExitCode; code == nil || *code != 0 {
		return "", filFailure("ssh key setup", job)
	}
	key := strings.TrimSpace(string(job.GetStdout()))
	if key == "" {
		return "", errors.New("the machine printed no public key; try again, or check the machine with agent_exec")
	}
	return key + "\n(add this to GitHub or authorized_keys; the private key never leaves the machine)", nil
}

// filRunJob starts command in the VM and polls it until it finishes or the budget runs out. The budget is
// min(requested seconds, the server's MaxWait); no single downstream call waits longer than s.waitSeconds allows.
// It returns the last output seen and whether the job has finished.
func (s *Server) filRunJob(ctx context.Context, vmID, command string, requested int) (*vmdv1.JobOutput, bool, error) {
	if requested < 0 {
		requested = 0
	}
	budget := min(time.Duration(requested)*time.Second, s.cfg.MaxWait)
	deadline := time.Now().Add(budget)
	start, err := s.cfg.Clients.Exec.StartJob(ctx, connect.NewRequest(&vmdv1.StartJobRequest{
		VmId:           vmID,
		Command:        command,
		TimeoutSeconds: filFetchTimeout,
		WaitSeconds:    int32(s.waitSeconds(requested)),
	}))
	if err != nil {
		return nil, false, err
	}
	out := start.Msg
	if out.GetJob().GetId() == "" {
		return nil, false, errors.New("the machine did not return a job for the command")
	}
	for !filJobFinal(out.GetJob().GetState()) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		next, err := s.cfg.Clients.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{
			JobId:       out.GetJob().GetId(),
			WaitSeconds: int32(s.waitSeconds(int(math.Ceil(remaining.Seconds())))),
			MaxOutput:   8192,
			Tail:        true,
		}))
		if err != nil {
			return nil, false, err
		}
		out = next.Msg
	}
	return out, filJobFinal(out.GetJob().GetState()), nil
}

// filFailure explains a finished job that did not succeed: its exit code and the tail of its stderr, or the reason
// it ended without an exit code.
func filFailure(what string, out *vmdv1.JobOutput) error {
	job := out.GetJob()
	if job.ExitCode == nil {
		reason := job.GetKilledReason()
		if reason == "" {
			reason = "no exit code"
		}
		return fmt.Errorf("%s failed: job %s ended as %s (%s)", what, job.GetId(), filStateName(job.GetState()), reason)
	}
	return fmt.Errorf("%s failed with exit code %d: %s", what, *job.ExitCode, strings.TrimSpace(string(filTail(out.GetStderr(), filStderrTail))))
}

// filFetchCommand is the shell command the VM runs for agent_fetch.
func filFetchCommand(path, rawURL string) string {
	q := shellQuote(path)
	return "curl -fsSL --create-dirs -o " + q + " " + shellQuote(rawURL) + " && ls -l " + q
}

// shellQuote quotes s as one POSIX shell word: single quotes, with each embedded single quote written as '\”.
// Nothing inside single quotes is expanded, so quotes, spaces, $(...), backticks and newlines stay literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// filParseMode reads an octal permission such as "0755". An empty string means the default 0644.
func filParseMode(mode string) (uint32, error) {
	if mode == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || v > 0o7777 {
		return 0, fmt.Errorf("mode %q is not an octal permission such as 0644 or 0755", mode)
	}
	if v == 0 {
		return 0, errors.New("mode 0000 is not allowed (the machine would use 0644 instead); use e.g. 0600 for a private file")
	}
	return uint32(v), nil
}

// filDecodeContent returns the bytes to upload. Base64 input may be line-wrapped, as base64(1) prints it.
func filDecodeContent(content string, isBase64 bool) ([]byte, error) {
	if !isBase64 {
		return []byte(content), nil
	}
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, content)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("content is not valid base64 (%v); fix the encoding, or send plain text with base64=false", err)
	}
	return data, nil
}

// filTrimPartialUTF8 removes an incomplete UTF-8 sequence at the end of b (left by a cut at max_bytes).
func filTrimPartialUTF8(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}

// filTail returns the last n bytes of b.
func filTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// filJobFinal reports whether a job state is terminal.
func filJobFinal(st vmdv1.JobState) bool {
	return st != vmdv1.JobState_JOB_STATE_RUNNING && st != vmdv1.JobState_JOB_STATE_UNSPECIFIED
}

// filStateName is the lower-case name of a job state, without the JOB_STATE_ prefix.
func filStateName(st vmdv1.JobState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "JOB_STATE_"))
}
