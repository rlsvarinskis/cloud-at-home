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
	"github.com/google/jsonschema-go/jsonschema"

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
	Path    string `json:"path" jsonschema:"full path of the file inside the machine, absolute, for example /home/agent/data.csv. It must not be an existing directory (see the tool description)"`
	Content string `json:"content" jsonschema:"file content: plain text, or base64 text when base64 is true (whitespace and line breaks in base64 are ignored). Empty is allowed. At most 8 MiB after decoding"`
	Base64  bool   `json:"base64,omitempty" jsonschema:"true when content is base64 of the file bytes (use for binary files); default false"`
	Mode    string `json:"mode,omitempty" jsonschema:"octal permission bits: 0001 to 7777, for example 0644 or 0755 (0000 is refused); default 0644"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type filDownloadArgs struct {
	Name     string `json:"name" jsonschema:"name of the machine (VM) to read the file from"`
	Path     string `json:"path" jsonschema:"path of the file inside the machine: absolute, or relative to the agent user's home. ~ is not expanded. Must be a regular file"`
	Encoding string `json:"encoding,omitempty" jsonschema:"auto (default: utf-8 if the file is valid UTF-8, else base64), utf-8 (fails on binary content) or base64 (the bytes as base64). Lower case"`
	Base64   bool   `json:"base64,omitempty" jsonschema:"deprecated alias for encoding=base64; used only when encoding is empty"`
	MaxBytes *int   `json:"max_bytes,omitempty" jsonschema:"most bytes to read: 1 to 8388608 (8 MiB); default 1048576 (1 MiB). utf-8 is capped at 1 MiB"`
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
	URL         string `json:"url" jsonschema:"http:// or https:// URL to download. The machine downloads it itself, so the bytes do not pass through this server. The whole URL, query string included, is stored as the job's command"`
	Path        string `json:"path" jsonschema:"where to save the file inside the machine: absolute, or relative to the agent user's home (~ is not expanded). An existing file is replaced; missing directories are created"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"how long this call waits for the download, in seconds (0 returns at once; default 45). Values above the server maximum wait are cut to it. If the download is still running, poll its job with agent_exec_poll"`
	Project     string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type filSSHPubkeyArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine (VM) whose SSH public key is wanted"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

// registerFileTools registers the file tools. Waits and caps in the text come from the server's MaxWait, and the
// schemas state the bounds the handlers enforce (the SDK checks them before a handler runs).
func (s *Server) registerFileTools() {
	maxWait := s.maxWaitText()
	machine := func(p map[string]*jsonschema.Schema) { schMinLen(schProp(p, "name"), 1) }

	addToolWithSchema(s, "agent_upload",
		"Write a file into machine `name`, as the agent user. path is the full path of the file: absolute, for example "+
			"/home/agent/data.csv. It must not be an existing directory: if it is one, the reply still says `wrote N bytes`, but the content "+
			"is written to a hidden .put.* file inside that directory. An existing file at path is replaced atomically. Missing parent "+
			"directories are created. content is plain text, or base64 text when base64 is true (use it for binary files). The limit is "+
			"8 MiB after decoding. mode sets the permission bits (default 0644). Reply: `wrote N bytes to <path>`. If the write fails inside "+
			"the machine (for example permission denied), the error reads `unavailable: node <id> is unavailable`; that text does not mean the "+
			"machine is down. The machine must be running.",
		toolInputSchema[filUploadArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			schMinLen(schProp(p, "path"), 1)
			schPattern(schProp(p, "path"), `^/`)
			schPattern(schProp(p, "mode"), `^[0-7]{1,4}$`)
		}),
		s.agentUpload)

	addToolWithSchema(s, "agent_download",
		"Read a file from machine `name`. Reply: a JSON object with path; size (bytes returned); sha256 (of the returned bytes, which is "+
			"not the whole file when truncated); encoding (utf-8 for text, base64 for bytes); content (the text, or the bytes as base64); and "+
			"truncated (present and true only when the read was cut at max_bytes). encoding auto (default) gives utf-8 when the file is valid "+
			"UTF-8 and base64 otherwise; utf-8 fails on binary content with 'is not UTF-8 text; call again with encoding=base64'; base64 always "+
			"gives base64. max_bytes limits the read: default 1 MiB; utf-8 is capped at 1 MiB, auto and base64 at 8 MiB. A truncated text read "+
			"may be a few bytes shorter than max_bytes so that no character is cut. Errors: not_found when there is no regular file at path "+
			"(a directory is not a file). A read the agent user is not allowed to do reads `unavailable: node <id> is unavailable`, which "+
			"does not mean the machine is down. The machine must be running.",
		toolInputSchema[filDownloadArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			schMinLen(schProp(p, "path"), 1)
			schEnum(schProp(p, "encoding"), "auto", "utf-8", "base64")
			schRange(schProp(p, "max_bytes"), 1, filMaxFileBytes)
		}),
		s.agentDownload)

	addToolWithSchema(s, "agent_fetch",
		fmt.Sprintf("Download an http:// or https:// URL inside machine `name`. The machine runs curl itself (curl must be installed there), "+
			"so the bytes do not pass through this server. The download is a job on the machine with a limit of %d s; a download that runs "+
			"longer is killed (exit code 124). It saves to path, creating directories, and replaces an existing file. Redirects are followed; "+
			"an HTTP status of 400 or more fails the download. The call waits up to wait_seconds (default 45 s, cut to the server maximum wait "+
			"of %s). Reply, as plain text: `ok: ` and the ls -l line of the saved file when it finished; `download still running as job <id>: "+
			"poll it with agent_exec_poll` otherwise (agent_exec_poll takes the same job_id and returns the agent_exec JSON object). Failure is an "+
			"error with the exit code and the last 1000 bytes of stderr. The command, URL included, is recorded as the job's command. "+
			"The machine must be running.",
			filFetchTimeout, maxWait),
		toolInputSchema[filFetchArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			schMinLen(schProp(p, "url"), 1)
			schPattern(schProp(p, "url"), `^[Hh][Tt][Tt][Pp][Ss]?://`)
			schMinLen(schProp(p, "path"), 1)
			schMin(schProp(p, "wait_seconds"), 0)
		}),
		s.agentFetch)

	addToolWithSchema(s, "agent_ssh_pubkey",
		fmt.Sprintf("Return the SSH public key (ed25519) of the agent user on machine `name`. If the machine has no key pair yet, it creates "+
			"~/.ssh/id_ed25519 there first; this runs as a job on the machine, which agent_exec_list shows. The call waits up to %s. Reply: the "+
			"public key on one line, then a note line that it is for GitHub or authorized_keys. If the key is not ready within the wait, the reply "+
			"names a job to poll with agent_exec_poll instead. Failure is an error `ssh key setup failed` with the exit code and the stderr tail. "+
			"The private key never leaves the machine. The machine must be running.", s.waitFor(filSSHKeyWait)),
		toolInputSchema[filSSHPubkeyArgs](func(p map[string]*jsonschema.Schema) { machine(p) }),
		s.agentSSHPubkey)
}

// waitFor is how long a call with a fixed wait of requested seconds really waits, as text: the request cut to the
// server's maximum wait, e.g. "20 s".
func (s *Server) waitFor(requested int) string {
	secs := min(float64(requested), s.cfg.MaxWait.Seconds())
	return strconv.FormatFloat(secs, 'f', -1, 64) + " s"
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
