package main

import (
	"context"
	"embed"
	"io/fs"
	"os/signal"
	"syscall"
	"time"

	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/amir20/dozzle/internal/agent"
	"github.com/amir20/dozzle/internal/auth"
	"github.com/amir20/dozzle/internal/docker"
	"github.com/amir20/dozzle/internal/healthcheck"
	"github.com/amir20/dozzle/internal/support/cli"
	docker_support "github.com/amir20/dozzle/internal/support/docker"
	"github.com/amir20/dozzle/internal/web"
	"github.com/rs/zerolog/log"
)

//go:embed all:dist
var content embed.FS

//go:embed shared_cert.pem shared_key.pem
var certs embed.FS

//go:generate protoc --go_out=. --go-grpc_out=. --proto_path=./protos ./protos/rpc.proto ./protos/types.proto
func main() {
	cli.ValidateEnvVars(cli.Args{}, cli.AgentCmd{})
	args, subcommand := cli.ParseArgs()
	if subcommand != nil {
		switch subcommand.(type) {
		case *cli.AgentCmd:
			cli.StartAgent(args, certs)

		case *cli.HealthcheckCmd:
			files, err := os.ReadDir(".")
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to read directory")
			}

			agentAddress := ""
			for _, file := range files {
				if match, _ := filepath.Match("agent-*.addr", file.Name()); match {
					data, err := os.ReadFile(file.Name())
					if err != nil {
						log.Fatal().Err(err).Msg("Failed to read file")
					}
					agentAddress = string(data)
					break
				}
			}
			if agentAddress == "" {
				if err := healthcheck.HttpRequest(args.Addr, args.Base); err != nil {
					log.Fatal().Err(err).Msg("Failed to make request")
				}
			} else {
				certs, err := cli.ReadCertificates(certs)
				if err != nil {
					log.Fatal().Err(err).Msg("Could not read certificates")
				}
				if err := healthcheck.RPCRequest(agentAddress, certs); err != nil {
					log.Fatal().Err(err).Msg("Failed to make request")
				}
			}

		case *cli.GenerateCmd:
			cli.StartEvent(args, "", nil, "generate")
			if args.Generate.Username == "" || args.Generate.Password == "" {
				log.Fatal().Msg("Username and password are required")
			}

			buffer := auth.GenerateUsers(auth.User{
				Username: args.Generate.Username,
				Password: args.Generate.Password,
				Name:     args.Generate.Name,
				Email:    args.Generate.Email,
			}, true)

			if _, err := os.Stdout.Write(buffer.Bytes()); err != nil {
				log.Fatal().Err(err).Msg("Failed to write to stdout")
			}
		}

		os.Exit(0)
	}

	if args.AuthProvider != "none" && args.AuthProvider != "forward-proxy" && args.AuthProvider != "simple" {
		log.Fatal().Str("provider", args.AuthProvider).Msg("Invalid auth provider")
	}

	log.Info().Msgf("Dozzle version %s", args.Version())

	var multiHostService *docker_support.MultiHostService

	switch args.Mode {
	case "server":
		var localClient docker.Client
		localClient, multiHostService = cli.CreateMultiHostService(certs, args)
		if multiHostService.TotalClients() == 0 {
			log.Fatal().Msg("Could not connect to any Docker Engine")
		} else {
			log.Info().Int("clients", multiHostService.TotalClients()).Msg("Connected to Docker")
		}
		go cli.StartEvent(args, "server", localClient, "")
		srv := createServer(args, multiHostService)
		go func() {
			log.Info().Msgf("Accepting connections on %s", args.Addr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("failed to listen")
			}
		}()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		<-ctx.Done()
		stop()
		log.Info().Msg("shutting down gracefully, press Ctrl+C again to force")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error().Err(err).Msg("failed to shut down")
		}
		log.Debug().Msg("shut down complete")
	case "agent":
		cli.StartAgent(args, certs)
	case "swarm":
		localClient, err := docker.NewLocalClient(args.Filter, args.Hostname)
		if err != nil {
			log.Fatal().Err(err).Msg("Could not create docker client")
		}
		certs, err := cli.ReadCertificates(certs)
		if err != nil {
			log.Fatal().Err(err).Msg("Could not read certificates")
		}
		manager := docker_support.NewSwarmClientManager(localClient, certs)
		multiHostService = docker_support.NewMultiHostService(manager)
		log.Info().Msg("Starting in swarm mode")
		listener, err := net.Listen("tcp", ":7007")
		if err != nil {
			log.Fatal().Err(err).Msg("failed to listen")
		}
		server, err := agent.NewServer(localClient, certs, args.Version())
		if err != nil {
			log.Fatal().Err(err).Msg("failed to create agent")
		}
		go cli.StartEvent(args, "swarm", localClient, "")
		go func() {
			log.Info().Msgf("Dozzle agent version %s", args.Version())
			if err := server.Serve(listener); err != nil {
				log.Error().Err(err).Msg("failed to serve")
			}
		}()

	default:
		log.Fatal().Str("mode", args.Mode).Msg("Invalid mode")
	}
}

func createServer(args cli.Args, multiHostService *docker_support.MultiHostService) *http.Server {
	_, dev := os.LookupEnv("DEV")

	var provider web.AuthProvider = web.NONE
	var authorizer web.Authorizer
	if args.AuthProvider == "forward-proxy" {
		log.Debug().Msg("Using forward proxy authentication")
		provider = web.FORWARD_PROXY
		authorizer = auth.NewForwardProxyAuth(args.AuthHeaderUser, args.AuthHeaderEmail, args.AuthHeaderName)
	} else if args.AuthProvider == "simple" {
		log.Debug().Msg("Using simple authentication")
		provider = web.SIMPLE

		path, err := filepath.Abs("./data/users.yml")
		if err != nil {
			log.Fatal().Err(err).Msg("Could not get absolute path")
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Fatal().Msg("users.yml file does not exist")
		}

		log.Debug().Str("path", path).Msg("Reading users.yml file")

		db, err := auth.ReadUsersFromFile(path)
		if err != nil {
			log.Fatal().Err(err).Msg("Could not read users.yml file")
		}

		log.Debug().Int("users", len(db.Users)).Msg("Loaded users")
		authorizer = auth.NewSimpleAuth(db)
	}

	config := web.Config{
		Addr:        args.Addr,
		Base:        args.Base,
		Version:     args.Version(),
		Hostname:    args.Hostname,
		NoAnalytics: args.NoAnalytics,
		Dev:         dev,
		Authorization: web.Authorization{
			Provider:   provider,
			Authorizer: authorizer,
		},
		EnableActions: args.EnableActions,
	}

	assets, err := fs.Sub(content, "dist")
	if err != nil {
		log.Fatal().Err(err).Msg("Could not get sub filesystem")
	}

	if _, ok := os.LookupEnv("LIVE_FS"); ok {
		if dev {
			log.Info().Msg("Using live filesystem at ./public")
			assets = os.DirFS("./public")
		} else {
			log.Info().Msg("Using live filesystem at ./dist")
			assets = os.DirFS("./dist")
		}
	}

	if !dev {
		if _, err := assets.Open(".vite/manifest.json"); err != nil {
			log.Fatal().Msg("manifest.json not found")
		}
		if _, err := assets.Open("index.html"); err != nil {
			log.Fatal().Msg("index.html not found")
		}
	}

	return web.CreateServer(multiHostService, assets, config)
}
