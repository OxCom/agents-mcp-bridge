//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsGroup struct {
	mu       sync.Mutex
	job      windows.Handle
	cmd      *exec.Cmd
	assigned bool
	killed   bool
}

// NewProcessGroup returns a Windows process-group binding backed by a job
// object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
//
// This is stronger than the POSIX build, and deliberately so: the kernel kills
// every process in the job when the last handle to it closes, including when the
// bridge is terminated without a chance to run cleanup. That is the control
// docs/03-threat-model.md T22 names, and the reason NFR-6's "no guarantee after
// SIGKILL" caveat does not apply on this platform.
//
// A child cannot escape the job by calling CreateProcess itself: breakaway is
// not permitted unless JOB_OBJECT_LIMIT_BREAKAWAY_OK is set, and it is not set.
func NewProcessGroup() ProcessGroup { return &windowsGroup{} }

// Attach creates the job and marks the child to start suspended. It does NOT
// assign the process to the job, because the process does not exist yet —
// AfterStart completes the binding. See the PostStarter contract in platform.go.
//
// CREATE_SUSPENDED is what makes the binding race-free. The child is created
// with its primary thread suspended, so it has executed no instruction, and
// therefore spawned no grandchild, by the time AfterStart assigns it to the job.
// Assigning a running process would leave a window in which it could fork a
// descendant that the job never captures.
//
// CREATE_NEW_PROCESS_GROUP additionally detaches the child from the bridge's
// console group, so a Ctrl-C delivered to the bridge does not reach the agent
// out of band.
func (g *windowsGroup) Attach(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return errors.New("Attach called after Start: the child may already have spawned descendants")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		return errors.New("Attach called twice on the same process group")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("set kill-on-job-close: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP

	g.job = job
	g.cmd = cmd
	return nil
}

// AfterStart assigns the started child to the job and then resumes it. The
// caller must invoke it immediately after cmd.Start() returns successfully; the
// child is suspended until it does and will never run otherwise.
//
// Opening the process by pid is safe against pid reuse here because os/exec
// still holds the process handle, which pins the pid for as long as the Cmd is
// alive. The handle Go holds is not reachable from exec.Cmd, which is why a
// second one is opened rather than borrowed.
func (g *windowsGroup) AfterStart() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return errors.New("AfterStart called without Attach")
	}
	if g.cmd == nil || g.cmd.Process == nil {
		return errors.New("AfterStart called before the process started")
	}
	if g.assigned {
		return nil
	}
	pid := g.cmd.Process.Pid
	// #nosec G115 -- a Windows pid from os.Process is a DWORD widened to int by
	// the standard library; narrowing it back cannot lose information.
	upid := uint32(pid)

	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		upid,
	)
	if err != nil {
		return fmt.Errorf("open child process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		return fmt.Errorf("assign process %d to job: %w", pid, err)
	}
	g.assigned = true

	// Resume only after the assignment has succeeded. If resuming fails the
	// child stays suspended inside the job, where KillAll still reaps it.
	if err := resumeProcess(upid); err != nil {
		return fmt.Errorf("resume process %d: %w", pid, err)
	}
	return nil
}

// KillAll closes the job handle, which the kernel turns into a termination of
// every process still inside it. It is idempotent: a second call finds the job
// already closed and reports success, matching the POSIX build where killing an
// already-dead group is not an error.
func (g *windowsGroup) KillAll() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.killed || g.job == 0 {
		return nil // never attached, or already reaped
	}
	g.killed = true

	// A child that was attached but never assigned (Start failed, or AfterStart
	// was never called) is not in the job, so closing the job would leave it
	// suspended forever. Terminate it directly first.
	if !g.assigned && g.cmd != nil && g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
	}

	// TerminateJobObject is explicit rather than relying on the close alone, so
	// the kill does not depend on this being the last handle to the job.
	termErr := windows.TerminateJobObject(g.job, 1)
	closeErr := windows.CloseHandle(g.job)
	g.job = 0
	if termErr != nil && !errors.Is(termErr, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("terminate job object: %w", termErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close job object: %w", closeErr)
	}
	return nil
}

// resumeProcess resumes every thread of pid.
//
// Designed, not ported: Go's syscall.StartProcess closes the child's primary
// thread handle before returning (src/syscall/exec_windows.go), and
// syscall.SysProcAttr exposes no attribute list, so neither the thread handle
// nor PROC_THREAD_ATTRIBUTE_JOB_LIST is reachable from os/exec. Enumerating the
// process's threads through a Toolhelp snapshot is the remaining route.
//
// A freshly created suspended process has exactly one thread, so the loop
// resumes one thread in practice; it is written as a loop because the snapshot
// API offers no way to ask for "the primary thread".
func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("read first thread: %w", err)
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == pid {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open thread %d: %w", entry.ThreadID, err)
			}
			_, err = windows.ResumeThread(th)
			_ = windows.CloseHandle(th)
			if err != nil {
				return fmt.Errorf("resume thread %d: %w", entry.ThreadID, err)
			}
			resumed++
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return fmt.Errorf("walk threads: %w", err)
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no thread found for process %d", pid)
	}
	return nil
}
