package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/config"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/docker"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/generator"

	"go.uber.org/zap"
)

// DockerLoader generates caddy files from docker swarm information
type DockerLoader struct {
	options         *config.Options
	initialized     bool
	dockerClient    docker.Client
	generator       *generator.CaddyfileGenerator
	timer           *time.Timer
	skipEvents      bool
	lastCaddyfile   []byte
	lastJSONConfig  []byte
	lastVersion     int64
	serversVersions *StringInt64CMap
	serversUpdating *StringBoolCMap
}

// CreateDockerLoader creates a docker loader
func CreateDockerLoader(options *config.Options) *DockerLoader {
	return &DockerLoader{
		options:         options,
		serversVersions: newStringInt64CMap(),
		serversUpdating: newStringBoolCMap(),
	}
}

func logger() *zap.Logger {
	return caddy.Log().
		Named("docker-proxy")
}

// Start docker loader
func (dockerLoader *DockerLoader) Start() error {
	if !dockerLoader.initialized {
		dockerLoader.initialized = true
		log := logger()

		dockerClient, err := client.NewEnvClient()
		if err != nil {
			log.Error("Docker connection failed", zap.Error(err))
			return err
		}

		dockerPing, err := dockerClient.Ping(context.Background())
		if err != nil {
			log.Error("Docker ping failed", zap.Error(err))
			return err
		}

		dockerClient.NegotiateAPIVersionPing(dockerPing)

		wrappedClient := docker.WrapClient(dockerClient)

		dockerLoader.dockerClient = wrappedClient
		dockerLoader.generator = generator.CreateGenerator(
			wrappedClient,
			docker.CreateUtils(),
			dockerLoader.options,
		)

		log.Info(
			"Start",
			zap.String("CaddyfilePath", dockerLoader.options.CaddyfilePath),
			zap.String("LabelPrefix", dockerLoader.options.LabelPrefix),
			zap.Duration("PollingInterval", dockerLoader.options.PollingInterval),
			zap.Bool("ProcessCaddyfile", dockerLoader.options.ProcessCaddyfile),
			zap.Bool("ProxyServiceTasks", dockerLoader.options.ProxyServiceTasks),
			zap.String("IngressNetworks", fmt.Sprintf("%v", dockerLoader.options.IngressNetworks)),
		)

		dockerLoader.timer = time.AfterFunc(0, func() {
			dockerLoader.update()
		})

		go dockerLoader.monitorEvents()
	}

	return nil
}

func (dockerLoader *DockerLoader) monitorEvents() {
	for {
		dockerLoader.listenEvents()
		time.Sleep(30 * time.Second)
	}
}

func (dockerLoader *DockerLoader) listenEvents() {
	args := filters.NewArgs()
	args.Add("scope", "swarm")
	args.Add("scope", "local")
	args.Add("type", "service")
	args.Add("type", "container")
	args.Add("type", "config")

	context, cancel := context.WithCancel(context.Background())

	eventsChan, errorChan := dockerLoader.dockerClient.Events(context, types.EventsOptions{
		Filters: args,
	})

	log := logger()
	log.Info("Connecting to docker events")

ListenEvents:
	for {
		select {
		case event := <-eventsChan:
			if dockerLoader.skipEvents {
				continue
			}

			update := (event.Type == "container" && event.Action == "create") ||
				(event.Type == "container" && event.Action == "start") ||
				(event.Type == "container" && event.Action == "stop") ||
				(event.Type == "container" && event.Action == "die") ||
				(event.Type == "container" && event.Action == "destroy") ||
				(event.Type == "service" && event.Action == "create") ||
				(event.Type == "service" && event.Action == "update") ||
				(event.Type == "service" && event.Action == "remove") ||
				(event.Type == "config" && event.Action == "create") ||
				(event.Type == "config" && event.Action == "remove")

			if update {
				dockerLoader.skipEvents = true
				dockerLoader.timer.Reset(100 * time.Millisecond)
			}
		case err := <-errorChan:
			cancel()
			if err != nil {
				log.Error("Docker events error", zap.Error(err))
			}
			break ListenEvents
		}
	}
}

func (dockerLoader *DockerLoader) update() bool {
	dockerLoader.timer.Reset(dockerLoader.options.PollingInterval)
	dockerLoader.skipEvents = false

	// Don't cache the logger more globally, it can change based on config reloads
	log := logger()
	caddyfile, controlledServers := dockerLoader.generator.GenerateCaddyfile(log)

	caddyfileChanged := !bytes.Equal(dockerLoader.lastCaddyfile, caddyfile)

	dockerLoader.lastCaddyfile = caddyfile

	if caddyfileChanged {
		log.Info("New Caddyfile", zap.ByteString("caddyfile", caddyfile))

		adapter := caddyconfig.GetAdapter("caddyfile")

		configJSON, warn, err := adapter.Adapt(caddyfile, nil)

		if warn != nil {
			log.Warn("Caddyfile to json warning", zap.String("warn", fmt.Sprintf("%v", warn)))
		}

		if err != nil {
			log.Error("Failed to convert caddyfile into json config", zap.Error(err))
			return false
		}

		log.Info("New Config JSON", zap.ByteString("json", configJSON))

		dockerLoader.lastJSONConfig = configJSON
		dockerLoader.lastVersion++
	}

	var errorCounter uint64
	var wg sync.WaitGroup
	for _, server := range controlledServers {
		wg.Add(1)
		updateServer := server
		go func() {
			if (!(dockerLoader.updateServer(updateServer))) {
   				atomic.AddUint64(&errorCounter, 1)
			}
			wg.Done()
		}()
	}
	wg.Wait()
	if errorCounter > 0 {
		server := "localhost"
		log.Info("Retrying after failure", zap.String("server", server))
		dockerLoader.updateServer(server)
	}
	return true
}

func (dockerLoader *DockerLoader) updateServer(server string) bool {

	// Skip servers that are being updated already
	if dockerLoader.serversUpdating.Get(server) {
		return true
	}

	// Flag and unflag updating
	dockerLoader.serversUpdating.Set(server, true)
	defer dockerLoader.serversUpdating.Delete(server)

	version := dockerLoader.lastVersion

	// Skip servers that already have this version
	if dockerLoader.serversVersions.Get(server) >= version {
		return true
	}

	log := logger()
	log.Info("Sending configuration to", zap.String("server", server))

	url := "http://" + server + ":2019/load"

	postBody, err := addAdminListen(dockerLoader.lastJSONConfig, "tcp/"+server+":2019")
	if err != nil {
		log.Error("Failed to add admin listen to", zap.String("server", server), zap.Error(err))
		return false
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(postBody))
	if err != nil {
		log.Error("Failed to create request to", zap.String("server", server), zap.Error(err))
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		log.Error("Failed to send configuration to", zap.String("server", server), zap.Error(err))
		return false
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("Failed to read response from", zap.String("server", server), zap.Error(err))
		return false
	}

	if resp.StatusCode != 200 {
		log.Error("Error response from server", zap.String("server", server), zap.Int("status code", resp.StatusCode), zap.ByteString("body", bodyBytes))
		return false
	}

	dockerLoader.serversVersions.Set(server, version)

	log.Info("Successfully configured", zap.String("server", server))
	return true
}

func addAdminListen(configJSON []byte, listen string) ([]byte, error) {
	config := &caddy.Config{}
	err := json.Unmarshal(configJSON, config)
	if err != nil {
		return nil, err
	}
	config.Admin = &caddy.AdminConfig{
		Listen: listen,
	}
	return json.Marshal(config)
}
