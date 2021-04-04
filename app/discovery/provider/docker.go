package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	dc "github.com/fsouza/go-dockerclient"
	log "github.com/go-pkgz/lgr"
	"github.com/pkg/errors"

	"github.com/umputun/reproxy/app/discovery"
)

//go:generate moq -out docker_client_mock.go -skip-ensure -fmt goimports . DockerClient

// Docker provide watch compatible changes from containers
// and maps by default from ^/api/%s/(.*) to http://%s:%d/$1, i.e. http://example.com/api/my_container/something
// will be mapped to http://172.17.42.1:8080/something. Ip will be the internal ip of the container and port - exposed the one
// Alternatively labels can alter this. dpx.route sets source route, and dpx.dest sets the destination. Optional dpx.server enforces
// match by server name (hostname).
type Docker struct {
	DockerClient DockerClient
	Excludes     []string
	Network      string
}

// DockerClient defines interface listing containers and subscribing to events
type DockerClient interface {
	ListContainers(opts dc.ListContainersOptions) ([]dc.APIContainers, error)
	AddEventListener(listener chan<- *dc.APIEvents) error
}

// containerInfo is simplified docker.APIEvents for containers only
type containerInfo struct {
	ID     string
	Name   string
	TS     time.Time
	Labels map[string]string
	IP     string
	Port   int
}

var (
	upStatuses   = []string{"start", "restart"}
	downStatuses = []string{"die", "destroy", "stop", "pause"}
)

// Channel gets eventsCh with all containers events
func (d *Docker) Events(ctx context.Context) (res <-chan struct{}) {
	eventsCh := make(chan struct{})
	go func() {
		// loop over to recover from failed events call
		for {
			err := d.events(ctx, d.DockerClient, eventsCh) // publish events to eventsCh
			if err == context.Canceled || err == context.DeadlineExceeded {
				close(eventsCh)
				return
			}
			log.Printf("[WARN] docker events listener failed, restarted, %v", err)
			time.Sleep(100 * time.Millisecond) // prevent busy loop on restart event listener
		}
	}()
	return eventsCh
}

// List all containers and make url mappers
func (d *Docker) List() ([]discovery.UrlMapper, error) {
	containers, err := d.listContainers()
	if err != nil {
		return nil, err
	}

	var res []discovery.UrlMapper
	for _, c := range containers {
		srcURL := fmt.Sprintf("^/api/%s/(.*)", c.Name)
		destURL := fmt.Sprintf("http://%s:%d/$1", c.IP, c.Port)
		server := "*"
		if v, ok := c.Labels["dpx.route"]; ok {
			srcURL = v
		}
		if v, ok := c.Labels["dpx.dest"]; ok {
			destURL = fmt.Sprintf("http://%s:%d%s", c.IP, c.Port, v)
		}
		if v, ok := c.Labels["dpx.server"]; ok {
			server = v
		}
		srcRegex, err := regexp.Compile(srcURL)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid src regex %s", srcURL)
		}

		res = append(res, discovery.UrlMapper{Server: server, SrcMatch: srcRegex, Dst: destURL})
	}
	return res, nil
}

func (d *Docker) ID() discovery.ProviderID { return discovery.PIDocker }

// activate starts blocking listener for all docker events
// filters everything except "container" type, detects stop/start events and publishes signals to eventsCh
func (d *Docker) events(ctx context.Context, client DockerClient, eventsCh chan struct{}) error {
	dockerEventsCh := make(chan *dc.APIEvents)
	if err := client.AddEventListener(dockerEventsCh); err != nil {
		return errors.Wrap(err, "can't add even listener")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-dockerEventsCh:
			if !ok {
				return errors.New("events closed")
			}
			if ev.Type != "container" {
				continue
			}
			if !contains(ev.Status, upStatuses) && !contains(ev.Status, downStatuses) {
				continue
			}
			log.Printf("[DEBUG] api event %+v", ev)
			containerName := strings.TrimPrefix(ev.Actor.Attributes["name"], "/")

			if contains(containerName, d.Excludes) {
				log.Printf("[DEBUG] container %s excluded", containerName)
				continue
			}
			log.Printf("[INFO] new event %+v", ev)
			eventsCh <- struct{}{}
		}
	}
}

func (d *Docker) listContainers() (res []containerInfo, err error) {

	portExposed := func(c dc.APIContainers) (int, bool) {
		if len(c.Ports) == 0 {
			return 0, false
		}
		return int(c.Ports[0].PrivatePort), true
	}

	containers, err := d.DockerClient.ListContainers(dc.ListContainersOptions{All: false})
	if err != nil {
		return nil, errors.Wrap(err, "can't list containers")
	}
	log.Printf("[DEBUG] total containers = %d", len(containers))

	for _, c := range containers {
		if !contains(c.Status, upStatuses) {
			continue
		}
		containerName := strings.TrimPrefix(c.Names[0], "/")
		if contains(containerName, d.Excludes) {
			log.Printf("[DEBUG] container %s excluded", containerName)
			continue
		}

		var ip string
		for k, v := range c.Networks.Networks {
			if k == d.Network { // match on network name
				ip = v.IPAddress
				break
			}
		}
		if ip == "" {
			continue
		}

		port, ok := portExposed(c)
		if !ok {
			continue
		}

		ci := containerInfo{
			Name:   containerName,
			ID:     c.ID,
			TS:     time.Unix(c.Created/1000, 0),
			Labels: c.Labels,
			IP:     ip,
			Port:   port,
		}

		log.Printf("[DEBUG] running container added, %+v", ci)
		res = append(res, ci)
	}
	log.Print("[DEBUG] completed list")
	return res, nil
}

func contains(e string, s []string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}