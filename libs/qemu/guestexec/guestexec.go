// Package guestexec runs shell commands as detached background "jobs" inside a
// Linux guest and reads their state back.
//
// It is a Go port of the Python/bash job implementation in
// local-agent/gateway/gateway.py. A job is a directory <JobsDir>/<id>/ holding:
//
//	cmd          the command text (bash source)
//	started      unix time the job was launched
//	pgid         pid of the job's session leader (also its process group id)
//	stdout       the command's standard output
//	stderr       the command's standard error
//	exit_code    present once the job has finished; written atomically via mv -n
//	cancelled    present once Cancel was called
//
// The command runs in its own session (setsid) with its output going to files, so
// nothing waits on pipes: background children of the command never block Start
// and never keep the job looking RUNNING after the command itself has exited.
// Every shell script is sent to the guest through a Runner, which for production
// is ssh as the unprivileged guest user.
package guestexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

const (
	// defaultJobsDir is where jobs live inside the guest unless Exec.JobsDir is set.
	defaultJobsDir = "/var/tmp/mcp-jobs"
	// defaultMaxOutput is the per-stream byte limit of a poll when maxOutput is unset.
	defaultMaxOutput = 8 << 10
	// maxOutputCap is the largest per-stream byte limit a poll may ask for.
	maxOutputCap = 64 << 10
	// defaultMaxFile and maxFileCap bound GetFile reads.
	defaultMaxFile = 8 << 20
	maxFileCap     = 8 << 20
	// exitMissingJob is the status the launch script uses when the job id already exists.
	exitMissingJob = 17
	// exitNoFile is the status the get-file script uses when the path is not a regular file.
	exitNoFile = 3
)

var jobIDRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Runner runs a shell command string in the guest (for real: over ssh as user "agent")
// with stdin, returning its output.
type Runner interface {
	Run(ctx context.Context, command string, stdin []byte) (stdout, stderr []byte, exitCode int, err error)
}

// Spec describes a job to start.
type Spec struct {
	// JobID names the job directory; it must match ^[a-zA-Z0-9_-]{1,64}$.
	JobID string
	// Command is bash source; it may contain any quotes, newlines or unicode.
	Command string
	// Workdir is the directory the command runs in; empty means the user's home.
	// It is created with mkdir -p when missing.
	Workdir string
	// TimeoutSeconds is the command's total lifetime; it is killed (exit 124) when it
	// expires. Values below 1 are treated as 1.
	TimeoutSeconds int
}

// Exec starts, polls and cancels jobs and moves files inside one guest.
type Exec struct {
	// R runs the plumbing scripts in the guest.
	R Runner
	// JobsDir is the absolute directory holding job directories. Empty means
	// /var/tmp/mcp-jobs.
	JobsDir string
}

// launchScript creates the job directory, stores the command from stdin and starts it
// detached. Arguments: $1 jobs dir, $2 job id, $3 timeout seconds, $4 workdir.
const launchScript = `
D=$1; J=$1/$2; T=$3; W=$4
mkdir -p "$D" || exit 1
if ! mkdir "$J" 2>/dev/null; then cat >/dev/null; echo "job $2 already exists" >&2; exit 17; fi
cat > "$J/cmd"
[ -n "$W" ] || W=$HOME
mkdir -p "$W" 2>/dev/null && cd "$W" || { rm -rf "$J"; echo "cannot use workdir $W" >&2; exit 1; }
date +%s > "$J/started"
( setsid bash -c '
    J=$1; echo $$ > "$J/pgid"
    timeout -k 2 "$2" bash -lc "$(cat "$J/cmd")" </dev/null >"$J/stdout" 2>"$J/stderr"
    echo $? > "$J/exit_code.tmp" && mv -n "$J/exit_code.tmp" "$J/exit_code"
' _ "$J" "$T" </dev/null >/dev/null 2>&1 & )
cd "$D" && ls -1t | tail -n +51 | while read d; do [ -e "$d/exit_code" ] && rm -rf "$d"; done
exit 0
`

// pollScript prints one header line, then the base64 of the stdout and stderr chunks.
// Arguments: $1 jobs dir, $2 job id, $3 stdout offset, $4 stderr offset, $5 cap,
// $6 "tail" or "since".
//
// Header: state exit_code cancelled elapsed stdout_size stderr_size stdout_from stderr_from.
// A job is lost when it has a pgid, no exit_code, and its session leader is gone
// (or is a zombie). A plain kill therefore reads as LOST, while Cancel writes an exit code.
const pollScript = `
J=$1/$2; CAP=$5
[ -d "$J" ] || { echo missing; exit 0; }
state=running; ec=-
if [ -e "$J/exit_code" ]; then state=exited; ec=$(cat "$J/exit_code")
else
  p=$(cat "$J/pgid" 2>/dev/null)
  if [ -n "$p" ] && { [ ! -d "/proc/$p" ] || grep -q '^State:[[:space:]]*Z' "/proc/$p/status" 2>/dev/null; }; then state=lost; fi
fi
can=0; [ -e "$J/cancelled" ] && can=1
now=$(date +%s); st=$(cat "$J/started" 2>/dev/null || echo "$now")
os=$(stat -c %s "$J/stdout" 2>/dev/null || echo 0); es=$(stat -c %s "$J/stderr" 2>/dev/null || echo 0)
if [ "$6" = tail ]; then oo=$((os>CAP?os-CAP:0)); eo=$((es>CAP?es-CAP:0)); else oo=$3; eo=$4; fi
echo "$state $ec $can $((now-st)) $os $es $oo $eo"
tail -c +$((oo+1)) "$J/stdout" 2>/dev/null | head -c "$CAP" | base64 -w0; echo
tail -c +$((eo+1)) "$J/stderr" 2>/dev/null | head -c "$CAP" | base64 -w0; echo
`

// cancelScript kills the job's whole session (TERM, then KILL) and records exit code 130.
// Arguments: $1 jobs dir, $2 job id.
const cancelScript = `
J=$1/$2; [ -d "$J" ] || exit 0
[ -e "$J/exit_code" ] && exit 0
touch "$J/cancelled"
p=""
for i in $(seq 1 40); do
  p=$(cat "$J/pgid" 2>/dev/null); [ -n "$p" ] && break
  [ -e "$J/exit_code" ] && exit 0
  sleep 0.05
done
if [ -n "$p" ]; then
  pkill -TERM -s "$p"
  for i in 1 2 3 4 5 6 7 8; do pgrep -s "$p" >/dev/null || break; sleep 0.25; done
  pkill -KILL -s "$p"
fi
[ -e "$J/exit_code" ] || { echo 130 > "$J/exit_code.tmp"; mv -n "$J/exit_code.tmp" "$J/exit_code"; }
exit 0
`

// putScript writes stdin to path atomically (temp file in the same directory, then mv)
// with the given octal mode. Arguments: $1 path, $2 octal mode.
const putScript = `
p=$1; m=$2
d=$(dirname -- "$p")
mkdir -p -- "$d" || exit 1
t=$(mktemp -- "$d/.put.XXXXXX") || exit 1
if cat > "$t" && chmod "$m" "$t" && mv -f -- "$t" "$p"; then exit 0; fi
rm -f -- "$t"; exit 1
`

// getScript prints at most n bytes of the regular file at $1, or exits 3 when there is none.
// Arguments: $1 path, $2 byte limit.
const getScript = `
[ -f "$1" ] || exit 3
head -c "$2" -- "$1"
`

// Start launches the job described by s and returns once it is running detached.
// It fails if the job id already exists.
func (e *Exec) Start(ctx context.Context, s Spec) error {
	if err := checkJobID(s.JobID); err != nil {
		return err
	}
	timeout := s.TimeoutSeconds
	if timeout < 1 {
		timeout = 1
	}
	dir, err := e.jobsDir()
	if err != nil {
		return err
	}
	_, err = e.run(ctx, launchScript, []byte(s.Command), dir, s.JobID, strconv.Itoa(timeout), s.Workdir)
	if err != nil {
		return fmt.Errorf("start job %s: %w", s.JobID, err)
	}
	return nil
}

// Poll reports the state of a job and its output.
//
// In offset mode (tail false) it returns up to maxOutput bytes of each stream starting
// at stdoutOff and stderrOff. In tail mode it returns the LAST maxOutput bytes of each
// stream and sets Truncated when earlier bytes were omitted. StdoutFrom and StderrFrom
// give where each returned slice starts, and StdoutOffset and StderrOffset give where
// the next poll should start. More is set when output remains past the returned bytes.
// maxOutput defaults to 8192 and is capped at 65536. An unknown job yields State MISSING
// with no error.
func (e *Exec) Poll(ctx context.Context, jobID string, stdoutOff, stderrOff int64, maxOutput int, tail bool) (*nodev1.PollJobResponse, error) {
	if err := checkJobID(jobID); err != nil {
		return nil, err
	}
	dir, err := e.jobsDir()
	if err != nil {
		return nil, err
	}
	if stdoutOff < 0 {
		stdoutOff = 0
	}
	if stderrOff < 0 {
		stderrOff = 0
	}
	limit := maxOutput
	if limit <= 0 {
		limit = defaultMaxOutput
	}
	if limit > maxOutputCap {
		limit = maxOutputCap
	}
	mode := "since"
	if tail {
		mode = "tail"
	}
	out, err := e.run(ctx, pollScript, nil, dir, jobID,
		strconv.FormatInt(stdoutOff, 10), strconv.FormatInt(stderrOff, 10), strconv.Itoa(limit), mode)
	if err != nil {
		return nil, fmt.Errorf("poll job %s: %w", jobID, err)
	}
	if string(bytes.TrimSpace(out)) == "missing" {
		return &nodev1.PollJobResponse{State: nodev1.GuestJobState_GUEST_JOB_STATE_MISSING}, nil
	}
	return parsePoll(out, tail)
}

// parsePoll decodes the output of pollScript.
func parsePoll(out []byte, tail bool) (*nodev1.PollJobResponse, error) {
	parts := strings.SplitN(string(out), "\n", 4)
	if len(parts) < 3 {
		return nil, fmt.Errorf("poll: malformed output %q", out)
	}
	hdr := strings.Fields(parts[0])
	if len(hdr) != 8 {
		return nil, fmt.Errorf("poll: malformed header %q", parts[0])
	}
	state, ok := map[string]nodev1.GuestJobState{
		"running": nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING,
		"exited":  nodev1.GuestJobState_GUEST_JOB_STATE_EXITED,
		"lost":    nodev1.GuestJobState_GUEST_JOB_STATE_LOST,
	}[hdr[0]]
	if !ok {
		return nil, fmt.Errorf("poll: unknown state %q", hdr[0])
	}
	resp := &nodev1.PollJobResponse{State: state}
	if hdr[1] != "-" {
		code, err := strconv.ParseInt(hdr[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("poll: bad exit code %q: %w", hdr[1], err)
		}
		c := int32(code)
		resp.ExitCode = &c
	}
	resp.Cancelled = hdr[2] == "1"
	elapsed, err := strconv.ParseInt(hdr[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("poll: bad elapsed %q: %w", hdr[3], err)
	}
	resp.ElapsedSeconds = elapsed
	stdoutSize, _ := strconv.ParseInt(hdr[4], 10, 64)
	stderrSize, _ := strconv.ParseInt(hdr[5], 10, 64)
	stdoutFrom, _ := strconv.ParseInt(hdr[6], 10, 64)
	stderrFrom, _ := strconv.ParseInt(hdr[7], 10, 64)

	stdout, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("poll: decode stdout: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("poll: decode stderr: %w", err)
	}
	resp.Stdout = stdout
	resp.Stderr = stderr
	resp.StdoutFrom = stdoutFrom
	resp.StderrFrom = stderrFrom
	resp.StdoutOffset = stdoutFrom + int64(len(stdout))
	resp.StderrOffset = stderrFrom + int64(len(stderr))
	resp.More = resp.StdoutOffset < stdoutSize || resp.StderrOffset < stderrSize
	resp.Truncated = tail && (stdoutFrom > 0 || stderrFrom > 0)
	return resp, nil
}

// Cancel kills the job's whole process group and records exit code 130 and the
// cancelled marker. Cancelling a finished or unknown job is a no-op.
func (e *Exec) Cancel(ctx context.Context, jobID string) error {
	if err := checkJobID(jobID); err != nil {
		return err
	}
	dir, err := e.jobsDir()
	if err != nil {
		return err
	}
	if _, err := e.run(ctx, cancelScript, nil, dir, jobID); err != nil {
		return fmt.Errorf("cancel job %s: %w", jobID, err)
	}
	return nil
}

// PutFile writes content to path inside the guest, creating parent directories, and sets
// the file mode (0644 when mode is 0). The write is atomic: a temp file is renamed into place.
func (e *Exec) PutFile(ctx context.Context, path string, content []byte, mode uint32) error {
	if path == "" {
		return errs.Invalid("empty path")
	}
	if mode == 0 {
		mode = 0o644
	}
	if _, err := e.run(ctx, putScript, content, path, fmt.Sprintf("%o", mode&0o7777)); err != nil {
		return fmt.Errorf("put file %s: %w", path, err)
	}
	return nil
}

// GetFile reads the regular file at path. At most maxBytes (default and cap 8 MiB) are
// returned; truncated reports that the file is longer. A missing file yields errs.NotFound.
func (e *Exec) GetFile(ctx context.Context, path string, maxBytes int64) (content []byte, truncated bool, err error) {
	if path == "" {
		return nil, false, errs.Invalid("empty path")
	}
	limit := maxBytes
	if limit <= 0 {
		limit = defaultMaxFile
	}
	if limit > maxFileCap {
		limit = maxFileCap
	}
	out, err := e.run(ctx, getScript, nil, path, strconv.FormatInt(limit+1, 10))
	var xe *exitError
	if errors.As(err, &xe) && xe.code == exitNoFile {
		return nil, false, errs.NotFound("file %s", path)
	}
	if err != nil {
		return nil, false, fmt.Errorf("get file %s: %w", path, err)
	}
	if int64(len(out)) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// run executes script with args through the Runner. The script travels as a single
// shell-quoted argument of bash -c, and each argument is shell-quoted. Transport failures
// wrap errs.ErrUnavailable. A non-zero exit returns an error that includes stderr.
func (e *Exec) run(ctx context.Context, script string, stdin []byte, args ...string) ([]byte, error) {
	if e.R == nil {
		return nil, errs.Unavailable("no runner configured")
	}
	var b strings.Builder
	b.WriteString("bash -c ")
	b.WriteString(shellQuote(script))
	b.WriteString(" _")
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	stdout, stderr, code, err := e.R.Run(ctx, b.String(), stdin)
	if err != nil {
		return nil, errs.Unavailable("guest command failed: %v", err)
	}
	if code != 0 {
		msg := strings.TrimSpace(string(stderr))
		if code == exitMissingJob && strings.Contains(msg, "already exists") {
			return nil, errs.Exists("%s", msg)
		}
		return nil, &exitError{code: code, msg: msg}
	}
	return stdout, nil
}

// exitError is a plumbing script that exited non-zero, with its trimmed stderr.
type exitError struct {
	code int
	msg  string
}

// Error implements error.
func (e *exitError) Error() string {
	return fmt.Sprintf("guest script exited %d: %s", e.code, e.msg)
}

// jobsDir returns the configured jobs directory, which must be absolute.
func (e *Exec) jobsDir() (string, error) {
	dir := e.JobsDir
	if dir == "" {
		dir = defaultJobsDir
	}
	if !path.IsAbs(dir) {
		return "", errs.Invalid("jobs dir %q is not absolute", dir)
	}
	return dir, nil
}

// checkJobID validates a job id against ^[a-zA-Z0-9_-]{1,64}$.
func checkJobID(id string) error {
	if !jobIDRE.MatchString(id) {
		return errs.Invalid("bad job id %q", id)
	}
	return nil
}

// shellQuote returns s as one single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
