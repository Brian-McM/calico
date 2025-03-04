package container

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Container struct {
	EnvVars       []string
	ImageName     string
	Docker        *client.Client
	ID            string
	Addr          string
	HealthCheck   func() error
	HostNetworked bool
	HostMounts    []string
	ContainerHost string
}

func (c *Container) Start() error {
	var err error
	c.Docker, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("could not create docker client (%s)", err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info(fmt.Sprintf("Creating container for %s", c.ImageName))
	cfg := &container.Config{Env: c.EnvVars, Image: c.ImageName}

	var hostCfg *container.HostConfig
	if c.HostNetworked {
		hostCfg = &container.HostConfig{
			NetworkMode: "host",
			Binds:       c.HostMounts,
		}
	}

	createdResult, err := c.Docker.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "image-assurance-api-"+strconv.Itoa(rand.Int()))
	if err != nil {
		return fmt.Errorf("could not create %s container (%s)", c.ImageName, err.Error())
	}
	c.ID = createdResult.ID

	log.Info(fmt.Sprintf("Starting container for %s", c.ImageName))
	err = c.Docker.ContainerStart(ctx, c.ID, container.StartOptions{})
	if err != nil {
		return fmt.Errorf("could not start %s container (%s)", c.ImageName, err.Error())
	}

	log.Info(fmt.Sprintf("Inspecting container for %s", c.ImageName))
	containerJson, err := c.Docker.ContainerInspect(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("could not inspect %s container (%s)", c.ImageName, err.Error())
	}

	if c.HostNetworked {
		c.Addr = "127.0.0.1"
		if c.ContainerHost != "" {
			c.Addr = c.ContainerHost
		}
	} else {
		c.Addr = containerJson.NetworkSettings.IPAddress
		if c.Addr == "" {
			return fmt.Errorf("could not get host address for %s", c.ImageName)
		}
	}

	log.Info(fmt.Sprintf("Address for %s is %s", c.ImageName, c.Addr))
	log.Info(fmt.Sprintf("Container for %s is ready", c.ImageName))

	if c.HealthCheck != nil {
		return c.HealthCheck()
	}

	return nil
}

func (c *Container) Stop() error {
	ctx := context.Background()
	secs := int(time.Second.Seconds()) * 10
	if err := c.Docker.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &secs}); err != nil {
		return err
	}
	if err := c.Docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
		return err
	}
	return nil
}

func (c *Container) GetAddr() string {
	return c.Addr
}

// Logs returns the logs from this container.
func (c *Container) Logs(since time.Time) []byte {
	stdBuff := bytes.NewBuffer([]byte{})
	errBuff := bytes.NewBuffer([]byte{})

	err := func() error {
		data, err := c.Docker.ContainerLogs(context.Background(), c.ID, container.LogsOptions{Since: since.Format(time.RFC3339Nano), ShowStdout: true, ShowStderr: true})
		if err != nil {
			return err
		}
		defer data.Close()

		_, err = stdcopy.StdCopy(stdBuff, errBuff, data)
		return err
	}()

	if err != nil {
		panic(err)
	}

	return stdBuff.Bytes()
}

func (c *Container) WriteLogs(since *time.Time) {
	stdBuff, errBuff, err := getContainerLogs(context.Background(), c.Docker, c.ID, since)
	if err != nil {
		log.Errorf("Failed to get logs for container %s (image %s)", c.ID, c.ImageName)
	}

	writeContainerLogs(c.ImageName, c.ID, c.ImageName, stdBuff)
	if errBuff.Len() > 0 {
		writeContainerLogs("[ERROR LOGS] "+c.ImageName, c.ID, c.ImageName, errBuff)
	}
}

func getContainerLogs(ctx context.Context, dockerClient *client.Client, containerID string, since *time.Time) (*bytes.Buffer, *bytes.Buffer, error) {
	stdBuff := bytes.NewBuffer([]byte{})
	errBuff := bytes.NewBuffer([]byte{})

	opts := container.LogsOptions{ShowStdout: true, ShowStderr: true}
	if since != nil {
		opts.Since = since.Format(time.RFC3339Nano)
	}

	data, err := dockerClient.ContainerLogs(ctx, containerID, opts)
	if err != nil {
		return nil, nil, err
	}
	defer data.Close()

	_, err = stdcopy.StdCopy(stdBuff, errBuff, data)
	return stdBuff, errBuff, err
}

// noFormatFormatter is a formatter for logrus that writes the message out and does no formatting to it. This is needed
// for writing out container logs, as those logs are already formatted.
type noFormatFormatter struct{}

func (n noFormatFormatter) Format(entry *log.Entry) ([]byte, error) {
	var b *bytes.Buffer
	if entry.Buffer != nil {
		b = entry.Buffer
	} else {
		b = &bytes.Buffer{}
	}

	b.WriteString(entry.Message)
	b.WriteByte('\n')

	return b.Bytes(), nil
}

func writeContainerLogs(title, id, imageName string, buff *bytes.Buffer) {
	noQuoteLogger := log.New()
	noQuoteLogger.SetFormatter(&noFormatFormatter{})

	noQuoteLogger.Print()
	noQuoteLogger.Printf("---------------%s LOGS START (id: %s, image name: %s) --------------", title, id, imageName)
	noQuoteLogger.Print()
	noQuoteLogger.Print(buff.String())
	noQuoteLogger.Printf("---------------%s LOGS END--------------", title)
}
