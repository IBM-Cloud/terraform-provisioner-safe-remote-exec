package winrm

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Anil-CM/safe-remote-exec/communicator/remote"
	"github.com/hashicorp/terraform/terraform"
	"github.com/masterzen/winrm"
	"github.com/packer-community/winrmcp/winrmcp"
)

const (
	MaxTimeOut = "MAX_TIMEOUT"
)

// Communicator represents the WinRM communicator
type Communicator struct {
	connInfo *connectionInfo
	client   *winrm.Client
	endpoint *winrm.Endpoint
	rand     *rand.Rand
}

// New creates a new communicator implementation over WinRM.
func New(s *terraform.InstanceState) (*Communicator, error) {
	connInfo, err := parseConnectionInfo(s)
	if err != nil {
		return nil, err
	}

	endpoint := &winrm.Endpoint{
		Host:     connInfo.Host,
		Port:     connInfo.Port,
		HTTPS:    connInfo.HTTPS,
		Insecure: connInfo.Insecure,
	}
	if len(connInfo.CACert) > 0 {
		endpoint.CACert = []byte(connInfo.CACert)
	}

	comm := &Communicator{
		connInfo: connInfo,
		endpoint: endpoint,
		// Seed our own rand source so that script paths are not deterministic
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	return comm, nil
}

// Connect implementation of communicator.Communicator interface
func (c *Communicator) Connect(o terraform.UIOutput) error {
	if c.client != nil {
		return nil
	}

	params := winrm.DefaultParameters
	params.Timeout = formatDuration(c.Timeout())
	if c.connInfo.NTLM == true {
		params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	}

	client, err := winrm.NewClientWithParameters(
		c.endpoint, c.connInfo.User, c.connInfo.Password, params)
	if err != nil {
		return err
	}

	if o != nil {
		o.Output(fmt.Sprintf(
			"Connecting to remote host via WinRM...\n"+
				"  Host: %s\n"+
				"  Port: %d\n"+
				"  User: %s\n"+
				"  Password: %t\n"+
				"  HTTPS: %t\n"+
				"  Insecure: %t\n"+
				"  NTLM: %t\n"+
				"  CACert: %t",
			c.connInfo.Host,
			c.connInfo.Port,
			c.connInfo.User,
			c.connInfo.Password != "",
			c.connInfo.HTTPS,
			c.connInfo.Insecure,
			c.connInfo.NTLM,
			c.connInfo.CACert != "",
		))
	}

	log.Printf("[DEBUG] connecting to remote shell using WinRM")
	shell, err := client.CreateShell()
	if err != nil {
		log.Printf("[ERROR] error creating shell: %s", err)
		return err
	}

	err = shell.Close()
	if err != nil {
		log.Printf("[ERROR] error closing shell: %s", err)
		return err
	}

	if o != nil {
		o.Output("Connected!")
	}

	c.client = client

	return nil
}

// Disconnect implementation of communicator.Communicator interface
func (c *Communicator) Disconnect() error {
	c.client = nil
	return nil
}

// Timeout implementation of communicator.Communicator interface
func (c *Communicator) Timeout() time.Duration {
	return c.connInfo.TimeoutVal
}

// ScriptPath implementation of communicator.Communicator interface
func (c *Communicator) ScriptPath() string {
	return strings.Replace(
		c.connInfo.ScriptPath, "%RAND%",
		strconv.FormatInt(int64(c.rand.Int31()), 10), -1)
}

// Start implementation of communicator.Communicator interface
func (c *Communicator) Start(rc *remote.Cmd, timeout int) error {
	rc.Init()
	log.Printf("[DEBUG] starting remote command: %s", rc.Command)

	// TODO: make sure communicators always connect first, so we can get output
	// from the connection.
	if c.client == nil {
		log.Println("[WARN] winrm client not connected, attempting to connect")
		if err := c.Connect(nil); err != nil {
			return err
		}
	}

	t := os.Getenv(MaxTimeOut)
	if len(t) != 0 {
		mTimeout, err := strconv.Atoi(t)
		if err != nil {
			return err
		}
		if timeout < mTimeout && mTimeout != 0 {
			timeout = mTimeout
		}
		log.Println("max timeout configured: ", timeout)
	}

	ctx, _ := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	execComplete := make(chan struct{})

	//status, err := c.client.Run(rc.Command, rc.Stdout, rc.Stderr)
	shell, err := c.client.CreateShell()
	if err != nil {
		return err
	}
	//defer shell.Close()
	cmdHandler, err := shell.Execute(rc.Command)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(rc.Stdout, cmdHandler.Stdout)
	}()

	go func() {
		defer wg.Done()
		io.Copy(rc.Stderr, cmdHandler.Stderr)
	}()

	go func() {
		for {
			select {

			case <-execComplete:
				log.Println("Commnad execution completed successfully within timeout limit")
				return

			case <-ctx.Done():
				log.Println("Remote command execution is terminated due to execution timeout")
				cmdHandler.Close()
				shell.Close()
				return
			}
		}
	}()

	wg.Wait()
	cmdHandler.Wait()

	if err != nil {
		return err
	} else {
		defer close(execComplete)
		rc.SetExitStatus(cmdHandler.ExitCode(), err)
	}
	return nil
}

// Upload implementation of communicator.Communicator interface
func (c *Communicator) Upload(path string, input io.Reader) error {
	wcp, err := c.newCopyClient()
	if err != nil {
		return err
	}
	log.Printf("[DEBUG] Uploading file to '%s'", path)
	return wcp.Write(path, input)
}

// UploadScript implementation of communicator.Communicator interface
func (c *Communicator) UploadScript(path string, input io.Reader, timeout int) error {
	return c.Upload(path, input)
}

// UploadDir implementation of communicator.Communicator interface
func (c *Communicator) UploadDir(dst string, src string) error {
	log.Printf("[DEBUG] Uploading dir '%s' to '%s'", src, dst)
	wcp, err := c.newCopyClient()
	if err != nil {
		return err
	}
	return wcp.Copy(src, dst)
}

func (c *Communicator) newCopyClient() (*winrmcp.Winrmcp, error) {
	addr := fmt.Sprintf("%s:%d", c.endpoint.Host, c.endpoint.Port)

	config := winrmcp.Config{
		Auth: winrmcp.Auth{
			User:     c.connInfo.User,
			Password: c.connInfo.Password,
		},
		Https:                 c.connInfo.HTTPS,
		Insecure:              c.connInfo.Insecure,
		OperationTimeout:      c.Timeout(),
		MaxOperationsPerShell: 15, // lowest common denominator
	}

	if c.connInfo.NTLM == true {
		config.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	}

	if c.connInfo.CACert != "" {
		config.CACertBytes = []byte(c.connInfo.CACert)
	}

	return winrmcp.New(addr, &config)
}
