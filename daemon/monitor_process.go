package daemon
import (
	"io/ioutil"
	"os"
	"path/filepath"
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/reexec"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/autogen/dockerversion"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/daemon/execdriver/native"
	"github.com/docker/docker/daemon/execdriver"
	"strconv"
	"bytes"
	"net/http"
	"encoding/json"
)

const (
	docker_monitor = "docker_monitor"
	monitor_pid_file = "monitor.pid"
	start_status_file = "start_status"
	stop_status_file = "stop_status"

// TODO this needs to be addressed
	root = "/var/run/docker"
// TODO this needs to be addressed
	fakeSelf = "/usr/bin/docker"

	http_retry_times = 5
	http_retry_interval_second = 3

	notify_start_url = "http://127.0.0.1:2375/monitor/%s/start"
	notify_stop_url = "http://127.0.0.1:2375/monitor/%s/stop"
)

type dockerMonitor struct {
	commonMonitor
}

type StartStatus struct {
	Pid int
	Err string
	//TODO start time
}

type StopStatus struct  {
	execdriver.ExitStatus
	Err string
}

func init() {
	reexec.RegisterSelf(docker_monitor, reexecMonitor, fakeSelf)
}

func newDockerMonitor(container *Container, policy runconfig.RestartPolicy) *dockerMonitor {
	return &dockerMonitor{
		commonMonitor: commonMonitor{
			restartPolicy:    policy,
			container:        container,
			stopChan:         make(chan struct{}),
			startSignal:      make(chan struct{}),
		},
	}
}

var monitor *dockerMonitor

func reexecMonitor() {
	var (
		containerId = os.Args[1]
		root = os.Args[2]
	)
	container := &Container{
		CommonContainer: CommonContainer{
			ID:    containerId,
			State: NewState(),
			root: filepath.Join(root, containerId),
			StreamConfig: StreamConfig{
				stdout: broadcastwriter.New(),
				stderr: broadcastwriter.New(),
				stdinPipe: ioutils.NopWriteCloser(ioutil.Discard),
			},
		},
	}
	if err := dumpToDisk(container.root, monitor_pid_file, []byte(strconv.Itoa(os.Getpid()))); err != nil {
		fail("Error dump pid %v", err)
	}
	if err := container.FromDisk(); err != nil {
		fail("Error load config %v", err)
	}

	if err := container.readHostConfig(); err != nil {
		fail("Error load hostconfig %v", err)
	}

	if err := container.readCommand(); err != nil {
		fail("Error load command %v", err)
	}
	//TODO env in ProcessConfig.execCmd should be changed to ProcessConfig.env
	env := container.createDaemonEnvironment([]string{})
	container.command.ProcessConfig.Env = env
	monitor = newDockerMonitor(container, container.hostConfig.RestartPolicy)
	sysInitPath := filepath.Join(root, "init", fmt.Sprintf("dockerinit-%s", dockerversion.VERSION))
	execRoot := filepath.Join(root, "execdriver", "native")
	driver, err := native.NewDriver(execRoot, sysInitPath, []string{})
	if err != nil {
		fail("new native driver err %v", err)
	}
	pipes := execdriver.NewPipes(container.stdin, container.stdout, container.stderr, container.Config.OpenStdin)
	monitor.startTime = time.Now()
	var exitStatus execdriver.ExitStatus
	exitStatus, err = driver.Run(container.command, pipes, monitor.callback)
	var errStr string
	if err != nil {
		errStr = err.Error()
	}
	monitor.notifyStop(StopStatus{
		ExitStatus:  exitStatus,
		Err:         errStr,
	})
	if err != nil {
		fail("start container err %v", err)
	}
}

func fail(message string, args ...interface{}) {
	logrus.Printf("[monitor] "+message, args...)
	os.Exit(1)
}

func dumpToDisk(containerRoot, file string, data []byte) error {
	pidfile := filepath.Join(containerRoot, file)
	return ioutil.WriteFile(pidfile, data, 0666)
}

func (m dockerMonitor) callback(processConfig *execdriver.ProcessConfig, pid int) {
	logrus.Infof("[monitor] pid %d", pid)
	m.notifyStart(StartStatus{Pid: pid})
	if err := m.container.ToDisk(); err != nil {
		logrus.Debugf("%s", err)
	}
}

func (m dockerMonitor) notifyStart(status StartStatus) error {
	d, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return m.notifyDaemon(notify_start_url, start_status_file, d)
}

func (m dockerMonitor) notifyStop(status StopStatus) error {
	d, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return m.notifyDaemon(notify_stop_url, stop_status_file, d)
}

func (m dockerMonitor) notifyDaemon(url, file string, d []byte) error {
	if err := dumpToDisk(m.container.root, file, d); err != nil {
		return err
	}
	for i := 0; i < http_retry_times; i++ {
		if err := httpNotify(url, m.container.ID, d); err != nil {
			logrus.Infof("http notify daemon %s failed %v, retry %d times", string(d), err, i)
			if i == http_retry_times-1 {
				return err
			}
		} else {
			break
		}
		time.Sleep(http_retry_interval_second * time.Second)
	}
	return nil
}

func httpNotify(url, cid string, data []byte) error {
	contentReader := bytes.NewReader(data)
	req, err := http.NewRequest("POST", fmt.Sprintf(url, cid), contentReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return  err
	}
	if (resp.StatusCode == http.StatusNoContent) {
		return nil
	}
	body, _ := ioutil.ReadAll(resp.Body)
	return fmt.Errorf(string(body))
}

func (container *Container) startStatusPath() string {
	return filepath.Join(container.root, start_status_file)
}

func (container *Container) stopStatusPath() string {
	return filepath.Join(container.root, stop_status_file)
}

func (container *Container) loadStartStatus() StartStatus {
	pth := container.startStatusPath()
	_, err := os.Stat(pth)
	if os.IsNotExist(err) {
		return nil
	}
	f, err := os.Open(pth)
	if err != nil {
		logrus.Errorf("Error open path %s", pth)
		return nil
	}
	defer f.Close()
	status := StartStatus{}
	json.NewDecoder(f).Decode(&status)
	return status
}

func (container *Container) loadStopStatus() StopStatus {
	pth := container.stopStatusPath()
	_, err := os.Stat(pth)
	if os.IsNotExist(err) {
		return nil
	}
	f, err := os.Open(pth)
	if err != nil {
		logrus.Errorf("Error open path %s", pth)
		return nil
	}
	defer f.Close()
	status := StopStatus{}
	json.NewDecoder(f).Decode(&status)
	return status
}