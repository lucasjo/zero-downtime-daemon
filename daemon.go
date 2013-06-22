/*
 * Copyright (c) 2013, PinIdea Co. Ltd.
 * Tomasen <tomasen@gmail.com> & Reck Hou <reckhou@gmail.com>
 * All rights reserved.
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *     * Redistributions of source code must retain the above copyright
 *       notice, this list of conditions and the following disclaimer.
 *     * Redistributions in binary form must reproduce the above copyright
 *       notice, this list of conditions and the following disclaimer in the
 *       documentation and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE REGENTS AND CONTRIBUTORS "AS IS" AND ANY
 * EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
 * WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
 * DISCLAIMED. IN NO EVENT SHALL THE COMPANY AND CONTRIBUTORS BE LIABLE FOR ANY
 * DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
 * (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
 * LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
 * ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
 * SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

/* The idea is came from nginx and this post: http://blog.nella.org/?p=879
 * and code here: http://code.google.com/p/jra-go/source/browse/#hg%2Fcmd%2Fupgradable
 */

package gozd

import (
  "reflect"
  "flag"
  "syscall"
  "runtime"
  "os"
  "strconv"
  "fmt"
  "os/signal"
  "os/exec"
  "net"
  "container/list"
  "time"
  "errors"
  "path/filepath"
  "strings"
)

// caller's infomation & channel
var (
  openedGOZDConns = list.New()
  registeredGOZDHandler = make(map[string]*gozdHandler) // key = group name used by specific user defined handler in config file
  mainRoutineCommChan = make(chan int, 1)
)

var (
  inheritedFDName []string
  inheritedFDCnt = 3 // if inherit FD from parent, use this count instead
)

var (
  isDaemonized = false
  runningPID = -1
  gozdWorkPath = ""
)

func Daemonize() (c chan int, isSucceed bool) {
  // parse arguments
  flag.Parse()
  
  runningPID = os.Getpid()
  var err error
  gozdWorkPath, err = filepath.Abs("")
  if err != nil {
    LogErr(err.Error())
  }
  Log("gozdWorkPath: " + gozdWorkPath)
  if isDaemonized {
    LogErr("Daemon already daemonized.")
    return c, false
  }

  *optConfigFile, _ = filepath.Abs(*optConfigFile)

  // -conf parse config
  if (!parseConfigFile(*optConfigFile)) {
    LogErr("Config file read error!")
    usage()
    os.Exit(1)
  }

  // find master process id by conf
  infos, err := getRunningInfoByConf(*optConfigFile, gozdPrefix)
  var pid int
  infoCnt := len(infos)
  if (err != nil || infoCnt < 1) {
    pid = 0
  } else {
    pid, _ = strconv.Atoi(infos[0])
  }

  if *optInheritFDName != "" {
    inheritedFDName = strings.Split(*optInheritFDName, ",")
  }
  
  // send signal to the process that has same config
  switch (*optSendSignal) {
    case "stop":
    if (pid != 0) {
      isRunning := IsProcessRunning(pid)
      if (isRunning) {
        p := getRunningProcess(pid)
        p.Signal(syscall.SIGTERM)
        os.Exit(0)
      }
    }
    os.Exit(0)
    
    case "start", "":
      if (pid != 0) {
        isRunning := IsProcessRunning(pid)
        if isRunning {
          LogErr("Daemon already started.")
          os.Exit(1)
        }
      }
      startDaemon()
    case "reopen","reload":
      // find old process, send SIGTERM then exit self
      // the 'real' new process running later starts by old process received SIGTERM
      if (pid != 0) {
        isRunning := IsProcessRunning(pid)
        if isRunning {
          p := getRunningProcess(pid)
          p.Signal(syscall.SIGTERM)
        }
      }
      startDaemon()
  }

  // Handle OS signals
  // Set up channel on which to send signal notifications.
  // We must use a buffered channel or risk missing the signal
  // if we're not ready to receive when the signal is sent.
  cSignal := make(chan os.Signal, 1)
  go signalHandler(cSignal)
  c = mainRoutineCommChan
  isDaemonized = true
  return c, true
}

// Regist callback handler
func RegistHandler(groupName, functionName string, fn interface{}) (err error) {
  v := reflect.ValueOf(fn)

  defer func(v reflect.Value) {
    if e := recover(); e != nil {
      err = errors.New("Callback: [" + groupName + "]" + functionName + "is not callable.")
      return
    }

    if _, ok := optGroups[groupName]; !ok {
      err = errors.New("Group " + groupName + "not exist!")
      return
    }

    l, errListen := newGOZDListener(optGroups[groupName].mode, optGroups[groupName].listen, groupName)
    if errListen != nil {
      err = errListen
      return
    }

    newHandler := new(gozdHandler)
    newHandler.funcName = functionName
    newHandler.val = v
    newHandler.listener = l
    registeredGOZDHandler[groupName] = newHandler
    
    go startAcceptConn(groupName, l)
  }(v)

  v.Type().NumIn()
  return
}

// A work-around solution since os.Findprocess() not working for now.
func IsProcessRunning(pid int) bool {
  if err := syscall.Kill(pid, 0); err != nil {
    return false
  } else {
    return true
  }
}

func startDaemon() {
  Log("Start daemon!")
  if daemon(0, 0) != 0 {
    os.Exit(1) 
  }
  pid := os.Getpid()
  resetRunningInfoByConf(*optConfigFile, gozdPrefix)
  path := getRunningInfoPathByConf(*optConfigFile, gozdPrefix)
  writeStringToFile(path, strconv.Itoa(pid))
}

func usage() {
  Log("[command] -conf = [config file]")
  flag.PrintDefaults()
}

// This function is the implementation of daemon() in standard C library.
// It forks a child process which copys itself, then terminate itself to make child's PPID = 1
// For more detail, run "man 3 daemon"
// "nochdir" is deprecated since it will cause filepath.Abs() get wrong workpath after fork/exec()
func daemon(nochdir, noclose int) int {
  var ret, ret2 uintptr
  var err syscall.Errno

  // If running at foreground, don't fork and continue.
  // This is useful for development & debug
  if *optRunForeground == true {
    return 0
  }

  darwin := runtime.GOOS == "darwin"

  // already a daemon
  if syscall.Getppid() == 1 {
    return 0
  }

  // fork off the parent process
  ret, ret2, err = syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
  if err != 0 {
    LogErr(err.Error())
    return -1
  }

  runningPID = os.Getpid()
  pid_after_fork := strconv.Itoa(os.Getpid())
  ppid_after_fork := strconv.Itoa(syscall.Getppid())
  Log("PID after fork: " + pid_after_fork)
  Log("PPID after fork: " + ppid_after_fork)

  // failure
  if ret2 < 0 {
    LogErr("failure!"+ string(ret2))
    os.Exit(-1)
  }

  // handle exception for darwin
  if darwin && ret2 == 1 {
    ret = 0
  }

  if noclose == 0 && *optRunForeground == false {
      f, e := os.OpenFile("/dev/null", os.O_RDWR, 0)
      if e == nil {
        fd := f.Fd()
        syscall.Dup2(int(fd), int(os.Stdin.Fd()))
        syscall.Dup2(int(fd), int(os.Stdout.Fd()))
        syscall.Dup2(int(fd), int(os.Stderr.Fd()))
      } else {
        LogErr("Dup to /dev/null error!" + err.Error())
      }
  }

  // if we got a good PID, then we call exit the parent process.
  if ret > 0 {
    Log("Exit parent process PID: " + pid_after_fork)
    os.Exit(0)
  }
  
  /* Change the file mode mask */
  _ = syscall.Umask(0)

  // create a new SID for the child process
  s_ret, s_errno := syscall.Setsid()
  if s_errno != nil {
      LogErr("Error: syscall.Setsid errno: %d", s_errno)
  }
  if s_ret < 0 {
    LogErr("Set sid failed %d", s_ret)
    return -1
  }

  Log("PID:" + pid_after_fork + " after parent process exited: " + strconv.Itoa(syscall.Getpid()))
  Log("PPID:" + ppid_after_fork + " after parent process exited: " + strconv.Itoa(syscall.Getppid()))

  return 0
}

func startAcceptConn(groupName string, listener *gozdListener) {
  for {
    // Wait for a connection.
    conn, err := listener.Accept()
    if err != nil {
      LogErr(err.Error())
      return
    }
    // Handle the connection in a new goroutine.
    // The loop then returns to accepting, so that
    // multiple connections may be served concurrently.
    go callHandler(groupName, conn)
    runtime.Gosched()
  }  
}

func startNewInstance(actionToOldProcess string, newFDs []*os.File) (*os.Process) {
  exec_path, _ := exec.LookPath(os.Args[0])
  path, _ := filepath.Abs(exec_path)
  args := make([]string, 0)
  args = append(args, fmt.Sprintf("-s=%s", actionToOldProcess))
  args = append(args, fmt.Sprintf("-c=%s", *optConfigFile))
  if *optRunForeground == true {
    args = append(args, "-f")
  }

  if *optVerbose == true {
    args = append(args, "-v")
  }

  argFDStr := ""
  for i := 0; i < len(newFDs); i++ {
    v := newFDs[i]
    argFDStr += v.Name()
    if i < len(newFDs) - 1 {
      argFDStr += ","
    }
  }
  args = append(args, fmt.Sprintf("-i=%s", argFDStr))
  
  cmd := exec.Command(path, args...)
  cmd.Stdout = os.Stdout
  cmd.Stderr = os.Stderr
  cmd.ExtraFiles = newFDs
  
  err := cmd.Start()
  if err != nil {
    LogErr(err.Error())
    return nil
  }

  return cmd.Process
}

func callHandler(groupName string, params ...interface{}) {
  if _, ok := registeredGOZDHandler[groupName]; !ok {
    LogErr(groupName + " does not exist.")
    return
  }

  handler := registeredGOZDHandler[groupName]
  if len(params) != handler.val.Type().NumIn() {
    LogErr("Invalid param of[" + groupName + "]" + handler.funcName)
    return
  }
  
  in := make([]reflect.Value, len(params))
  for k, param := range params {
    in[k] = reflect.ValueOf(param)
  }
  
  handler.val.Call(in)
}

func signalHandler(cSignal chan os.Signal) {
  signal.Notify(cSignal, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR2, syscall.SIGINT)
  // Block until a signal is received.
  for s := range cSignal {
    Log("Got signal: %v", s)
    switch (s) {
      case syscall.SIGHUP, syscall.SIGUSR2:
        // upgrade, reopen
        // 1. write running process FDs into info config(*.gozd) before starting
        // 2. stop port listening
        // 3. using exec.Command() to start a new instance
        Log("Action: PREPARE TO STOP")
        newFDs := dupNetFDs()
        stopListening()
        child := startNewInstance("reopen", newFDs)
        handleChildExit(child)
      case syscall.SIGTERM, syscall.SIGINT:
        Log("Action: CLOSE")
        // wait all clients disconnect
        c := make(chan int , 1)
        go waitTillAllConnClosed(c)
        <- c
        // quit, send signal to let caller do cleanups
        mainRoutineCommChan <- 0
    }
  }
}

// A work-around solution to make a new os.Process since os.newProcess() is not public.
func getRunningProcess(pid int) (p *os.Process) {
  p, _ = os.FindProcess(pid)
  return p
}

// Dup old process's fd to new one, write FD & name into info file.
func dupNetFDs() (newFDs []*os.File) {
  newFDs = []*os.File{}
  resetRunningInfoByConf(*optConfigFile, gozdPrefix)
  for k, v := range registeredGOZDHandler {
    Log(k + "|%v", v)
    l := v.listener.Listener.(*net.TCPListener) // TODO: Support to net.UnixListener
    newFD, err := l.File() // net.TCPListener.File() call dup() to return a new FD
    if err == nil {
      newFDs = append(newFDs, newFD)
    } else {
      LogErr(err.Error())
    }
  }
  return newFDs
}

func waitTillAllConnClosed(c chan int) {
  for openedGOZDConns.Len() != 0 {
    time.Sleep(1 * time.Second)
  }
  Log("All conn closed.")
  c <- 1
}

// If not running foreground, parent should handle child process's exit status after child call Fork(), or a zombie process will occour.
// Notice: If running foreground, zombie process will occour due to program's main logic. This is not an issue.
func handleChildExit(process *os.Process) {
  if *optRunForeground == true || process == nil {
    return
  }

  state, err := process.Wait()
  if err != nil {
    LogErr(err.Error())
  } else {
    if state.Exited() == true {
      Log("Child %d exited", state.Pid())
    } else {
      LogErr("Child %d not exited", state.Pid())
    }
  }
}
