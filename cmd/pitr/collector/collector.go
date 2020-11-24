package collector

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"

	"github.com/minio/minio-go"
	"github.com/pkg/errors"

	"github.com/percona/percona-xtradb-cluster-operator/cmd/pitr/db"
	"github.com/percona/percona-xtradb-cluster-operator/cmd/pitr/storage"
)

type Collector struct {
	db             *db.PXC
	storage        Storage
	lastSet        string // last uploaded binary logs set
	pxcServiceName string // k8s service name for PXC, its for get correct host for connection
	pxcUser        string // user for connection to PXC
	pxcPass        string // password for connection to PXC
	bufferSize     int64  // size of uploading buffer
}

type Config struct {
	PXCServiceName string
	PXCUser        string
	PXCPass        string
	S3Endpoint     string
	S3AccessKeyID  string
	S3AccessKey    string
	S3BucketName   string
	S3Region       string
	BufferSize     int64
}

type Storage interface {
	GetObject(objectName string) (io.Reader, error)
	PutObject(name string, data io.Reader) error
}

const (
	lastSetFileName string = "last-binlog-set" // name for object where the last binlog set will stored
	gtidPostfix     string = "-gtid-set"       // filename postfix for files with GTID set
)

func New(c Config) (*Collector, error) {
	s3, err := storage.NewS3(c.S3Endpoint, c.S3AccessKeyID, c.S3AccessKey, c.S3BucketName, c.S3Region, true)
	if err != nil {
		return nil, errors.Wrap(err, "new storage manager")
	}

	// get last binlog set stored on S3
	lastSetObject, err := s3.GetObject(lastSetFileName)
	if err != nil {
		return nil, errors.Wrap(err, "get last set content")
	}
	lastSet, err := ioutil.ReadAll(lastSetObject)
	if err != nil && minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return nil, errors.Wrap(err, "read object")
	}
	return &Collector{
		storage:        s3,
		lastSet:        string(lastSet),
		pxcUser:        c.PXCUser,
		pxcPass:        c.PXCPass,
		pxcServiceName: c.PXCServiceName,
	}, nil
}

func (c *Collector) Run() error {
	err := c.newDB()
	if err != nil {
		return errors.Wrap(err, "new db connection")
	}
	defer c.closeDB()

	err = c.CollectBinLogs()
	if err != nil {
		return errors.Wrap(err, "collect binlog files:")
	}

	return nil
}

func (c *Collector) newDB() error {
	host, err := c.getHost()
	if err != nil {
		return errors.Wrap(err, "get host")
	}
	pxc, err := db.NewPXC(host, c.pxcUser, c.pxcPass)
	if err != nil {
		return errors.Wrapf(err, "new manager with host %s", host)

	}
	c.db = pxc

	return nil
}

func (c *Collector) closeDB() error {
	return c.db.Close()
}

func (c *Collector) getHost() (string, error) {
	cmd := exec.Command("peer-list", "-on-start=/usr/bin/get-pxc-state", "-service="+c.pxcServiceName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.Wrap(err, "get output")
	}
	nodes := strings.Split(string(out), "node:")
	sort.Strings(nodes)
	lastHost := ""
	for _, node := range nodes {
		if strings.Contains(node, "wsrep_ready:ON:wsrep_connected:ON:wsrep_local_state_comment:Synced:wsrep_cluster_status:Primary") {
			nodeArr := strings.Split(node, ":")
			lastHost = nodeArr[0]
		}
	}
	if len(lastHost) == 0 {
		return "", errors.New("cant find host")
	}

	return lastHost, nil
}

func (c *Collector) CollectBinLogs() error {
	// get last uploaded binlog file name
	binlogName, err := c.db.GetBinLogName(c.lastSet)
	if err != nil {
		return errors.Wrap(err, "get binlog name by set")
	}
	list, err := c.db.GetBinLogList()
	if err != nil {
		return errors.Wrap(err, "get binlog list")
	}

	upload := false
	// if there are no uploaded files we going to upload every binlog file
	if len(binlogName) == 0 {
		upload = true
	}

	for _, binlog := range list {
		if binlog == binlogName { // this check is for uploading starting from needed file
			upload = true
		}
		if upload {
			err = c.manageBinlog(binlog)
			if err != nil {
				return errors.Wrap(err, "manage binlog")
			}
		}
	}

	return nil
}

type reader struct {
	r io.Reader
}

func (r *reader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (c *Collector) manageBinlog(binlog string) error {
	set, err := c.db.GetGTIDSet(binlog)
	if err != nil {
		return errors.Wrap(err, "get GTID set")
	}
	var setBuffer bytes.Buffer
	setBuffer.WriteString(set)

	if setBuffer.Len() == 0 {
		return nil
	}

	os.Remove("/tmp/" + binlog)

	err = syscall.Mkfifo("/tmp/"+binlog, 0666)
	if err != nil {
		return errors.Wrap(err, "make named pipe file error")
	}

	file, err := os.OpenFile("/tmp/"+binlog, syscall.O_NONBLOCK, os.ModeNamedPipe)
	if err != nil {
		return errors.Wrap(err, "open named pipe file error:")
	}
	defer file.Close()

	cmdStr := "mysqlbinlog -R --raw" + " -h" + c.db.GetHost() + " -u" + c.pxcUser + " -p$PXC_PASS --result-file=/tmp/ " + binlog
	cmd := exec.Command("sh", "-c", cmdStr)

	errOut, err := cmd.StderrPipe()
	if err != nil {
		return errors.Wrap(err, "get stderr pipe")
	}

	err = cmd.Start()
	if err != nil {
		return errors.Wrap(err, "run mysqlbinlog command")
	}
	stdErr, err := ioutil.ReadAll(errOut)
	if err != nil {
		return errors.Wrap(err, "read error output")
	}

	if stdErr != nil && string(bytes.TrimRight(stdErr, "\n")) != db.UsingPassErrorMessage {
		return errors.Errorf("mysqlbinlog: %s", stdErr)
	}

	data := reader{file}
	err = c.storage.PutObject(binlog, &data)
	if err != nil {
		return errors.Wrap(err, "put binlog object")
	}

	cmd.Wait()
	err = os.Remove("/tmp/" + binlog)
	if err != nil {
		return errors.Wrap(err, "remove file")
	}

	err = c.storage.PutObject(binlog+gtidPostfix, &setBuffer)
	if err != nil {
		return errors.Wrap(err, "put gtid-set object")
	}

	setBuffer.WriteString(set)
	err = c.storage.PutObject(lastSetFileName, &setBuffer)
	if err != nil {
		return errors.Wrap(err, "put last-set object")
	}
	c.lastSet = set

	return nil
}
