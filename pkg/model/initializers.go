package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	grpc "github.com/go-skynet/LocalAI/pkg/grpc"
	"github.com/phayes/freeport"
	"github.com/rs/zerolog/log"
	"golang.org/x/sys/cpu"
)

var Aliases map[string]string = map[string]string{
	"go-llama":              LLamaCPP,
	"llama":                 LLamaCPP,
	"embedded-store":        LocalStoreBackend,
	"langchain-huggingface": LCHuggingFaceBackend,
}

const (
	LlamaGGML = "llama-ggml"

	LLamaCPP  = "llama-cpp"
	LLamaCPPCUDA12 = "llama-cpp-cuda12"
	LLamaCPPAVX2 = "llama-cpp-avx2"
	LLamaCPPAVX = "llama-cpp-avx"
	LLamaCPPFallback = "llama-cpp-fallback"

	Gpt4AllLlamaBackend = "gpt4all-llama"
	Gpt4AllMptBackend   = "gpt4all-mpt"
	Gpt4AllJBackend     = "gpt4all-j"
	Gpt4All             = "gpt4all"

	BertEmbeddingsBackend  = "bert-embeddings"
	RwkvBackend            = "rwkv"
	WhisperBackend         = "whisper"
	StableDiffusionBackend = "stablediffusion"
	TinyDreamBackend       = "tinydream"
	PiperBackend           = "piper"
	LCHuggingFaceBackend   = "huggingface"

	LocalStoreBackend = "local-store"
)

func backendPath(assetDir, backend string) string {
	return filepath.Join(assetDir, "backend-assets", "grpc", backend)
}

// backendsInAssetDir returns the list of backends in the asset directory
// that should be loaded
func backendsInAssetDir(assetDir string) ([]string, error) {
	// Exclude backends from automatic loading
	excludeBackends := []string{LocalStoreBackend}
	entry, err := os.ReadDir(backendPath(assetDir, ""))
	if err != nil {
		return nil, err
	}
	var backends []string
ENTRY:
	for _, e := range entry {
		for _, exclude := range excludeBackends {
			if e.Name() == exclude {
				continue ENTRY
			}
		}
		if !e.IsDir() {
			backends = append(backends, e.Name())
		}
	}

	// order backends from the asset directory.
	// as we scan for backends, we want to keep some order which backends are tried of.
	// for example, llama.cpp should be tried first, and we want to keep the huggingface backend at the last.
	// sets a priority list
	// First has more priority
	priorityList := []string{
		// First llama.cpp and llama-ggml
		LLamaCPP, LLamaCPPFallback, LlamaGGML, Gpt4All,
	}
	toTheEnd := []string{
		// last has to be huggingface
		LCHuggingFaceBackend,
		// then bert embeddings
		BertEmbeddingsBackend,
	}
	slices.Reverse(priorityList)
	slices.Reverse(toTheEnd)

	// order certain backends first
	for _, b := range priorityList {
		for i, be := range backends {
			if be == b {
				backends = append([]string{be}, append(backends[:i], backends[i+1:]...)...)
				break
			}
		}
	}
	// make sure that some others are pushed at the end
	for _, b := range toTheEnd {
		for i, be := range backends {
			if be == b {
				backends = append(append(backends[:i], backends[i+1:]...), be)
				break
			}
		}
	}

	return backends, nil
}

// starts the grpcModelProcess for the backend, and returns a grpc client
// It also loads the model
func (ml *ModelLoader) grpcModel(backend string, o *Options) func(string, string) (ModelAddress, error) {
	return func(modelName, modelFile string) (ModelAddress, error) {
		log.Debug().Msgf("Loading Model %s with gRPC (file: %s) (backend: %s): %+v", modelName, modelFile, backend, *o)

		var client ModelAddress

		getFreeAddress := func() (string, error) {
			port, err := freeport.GetFreePort()
			if err != nil {
				return "", fmt.Errorf("failed allocating free ports: %s", err.Error())
			}
			return fmt.Sprintf("127.0.0.1:%d", port), nil
		}

		// If no specific model path is set for transformers/HF, set it to the model path
		for _, env := range []string{"HF_HOME", "TRANSFORMERS_CACHE", "HUGGINGFACE_HUB_CACHE"} {
			if os.Getenv(env) == "" {
				err := os.Setenv(env, ml.ModelPath)
				if err != nil {
					log.Error().Err(err).Str("name", env).Str("modelPath", ml.ModelPath).Msg("unable to set environment variable to modelPath")
				}
			}
		}

		// Check if the backend is provided as external
		if uri, ok := o.externalBackends[backend]; ok {
			log.Debug().Msgf("Loading external backend: %s", uri)
			// check if uri is a file or a address
			if _, err := os.Stat(uri); err == nil {
				serverAddress, err := getFreeAddress()
				if err != nil {
					return "", fmt.Errorf("failed allocating free ports: %s", err.Error())
				}
				// Make sure the process is executable
				if err := ml.startProcess(uri, o.model, serverAddress); err != nil {
					return "", err
				}

				log.Debug().Msgf("GRPC Service Started")

				client = ModelAddress(serverAddress)
			} else {
				// address
				client = ModelAddress(uri)
			}
		} else {
			grpcProcess := backendPath(o.assetDir, backend)
			// Check if the file exists
			if _, err := os.Stat(grpcProcess); os.IsNotExist(err) {
				return "", fmt.Errorf("grpc process not found: %s. some backends(stablediffusion, tts) require LocalAI compiled with GO_TAGS", grpcProcess)
			}

			serverAddress, err := getFreeAddress()
			if err != nil {
				return "", fmt.Errorf("failed allocating free ports: %s", err.Error())
			}

			// Make sure the process is executable
			if err := ml.startProcess(grpcProcess, o.model, serverAddress); err != nil {
				return "", err
			}

			log.Debug().Msgf("GRPC Service Started")

			client = ModelAddress(serverAddress)
		}

		// Wait for the service to start up
		ready := false
		for i := 0; i < o.grpcAttempts; i++ {
			alive, err := client.GRPC(o.parallelRequests, ml.wd).HealthCheck(context.Background())
			if alive {
				log.Debug().Msgf("GRPC Service Ready")
				ready = true
				break
			}
			if err != nil && i == o.grpcAttempts-1 {
				log.Error().Err(err).Msg("failed starting/connecting to the gRPC service")
			}
			time.Sleep(time.Duration(o.grpcAttemptsDelay) * time.Second)
		}

		if !ready {
			log.Debug().Msgf("GRPC Service NOT ready")
			return "", fmt.Errorf("grpc service not ready")
		}

		options := *o.gRPCOptions
		options.Model = modelName
		options.ModelFile = modelFile

		log.Debug().Msgf("GRPC: Loading model with options: %+v", options)

		res, err := client.GRPC(o.parallelRequests, ml.wd).LoadModel(o.context, &options)
		if err != nil {
			return "", fmt.Errorf("could not load model: %w", err)
		}
		if !res.Success {
			return "", fmt.Errorf("could not load model (no success): %s", res.Message)
		}

		return client, nil
	}
}

func (ml *ModelLoader) resolveAddress(addr ModelAddress, parallel bool) (grpc.Backend, error) {
	if parallel {
		return addr.GRPC(parallel, ml.wd), nil
	}

	if _, ok := ml.grpcClients[string(addr)]; !ok {
		ml.grpcClients[string(addr)] = addr.GRPC(parallel, ml.wd)
	}
	return ml.grpcClients[string(addr)], nil
}

func (ml *ModelLoader) BackendLoader(opts ...Option) (client grpc.Backend, err error) {
	o := NewOptions(opts...)

	if o.model != "" {
		log.Info().Msgf("Loading model '%s' with backend %s", o.model, o.backendString)
	} else {
		log.Info().Msgf("Loading model with backend %s", o.backendString)
	}

	backend := strings.ToLower(o.backendString)
	if realBackend, exists := Aliases[backend]; exists {
		backend = realBackend
		log.Debug().Msgf("%s is an alias of %s", backend, realBackend)
	}

	if o.singleActiveBackend {
		ml.mu.Lock()
		log.Debug().Msgf("Stopping all backends except '%s'", o.model)
		err := ml.StopAllExcept(o.model)
		ml.mu.Unlock()
		if err != nil {
			log.Error().Err(err).Str("keptModel", o.model).Msg("error while shutting down all backends except for the keptModel")
			return nil, err
		}

	}

	var backendToConsume string

	switch backend {
	case Gpt4AllLlamaBackend, Gpt4AllMptBackend, Gpt4AllJBackend, Gpt4All:
		o.gRPCOptions.LibrarySearchPath = filepath.Join(o.assetDir, "backend-assets", "gpt4all")
		backendToConsume = Gpt4All
	case PiperBackend:
		o.gRPCOptions.LibrarySearchPath = filepath.Join(o.assetDir, "backend-assets", "espeak-ng-data")
		backendToConsume = PiperBackend
	default:
		backendToConsume = backend
	}

	addr, err := ml.LoadModel(o.model, ml.grpcModel(backendToConsume, o))
	if err != nil {
		return nil, err
	}

	return ml.resolveAddress(addr, o.parallelRequests)
}

func (ml *ModelLoader) GreedyLoader(opts ...Option) (grpc.Backend, error) {
	o := NewOptions(opts...)

	ml.mu.Lock()
	// Return earlier if we have a model already loaded
	// (avoid looping through all the backends)
	if m := ml.CheckIsLoaded(o.model); m != "" {
		log.Debug().Msgf("Model '%s' already loaded", o.model)
		ml.mu.Unlock()

		return ml.resolveAddress(m, o.parallelRequests)
	}
	// If we can have only one backend active, kill all the others (except external backends)
	if o.singleActiveBackend {
		log.Debug().Msgf("Stopping all backends except '%s'", o.model)
		err := ml.StopAllExcept(o.model)
		if err != nil {
			log.Error().Err(err).Str("keptModel", o.model).Msg("error while shutting down all backends except for the keptModel - greedyloader continuing")
		}
	}
	ml.mu.Unlock()

	var err error

	// autoload also external backends
	allBackendsToAutoLoad := []string{}
	autoLoadBackends, err := backendsInAssetDir(o.assetDir)
	if err != nil {
		return nil, err
	}
	log.Debug().Msgf("Loading from the following backends (in order): %+v", autoLoadBackends)
	allBackendsToAutoLoad = append(allBackendsToAutoLoad, autoLoadBackends...)
	for _, b := range o.externalBackends {
		allBackendsToAutoLoad = append(allBackendsToAutoLoad, b)
	}

	// SERTAC
	for i, v := range allBackendsToAutoLoad {
		if v == "llama-cpp" {
			if cpu.X86.HasAVX2 {
				allBackendsToAutoLoad[i] = LLamaCPPAVX2
			} else if cpu.X86.HasAVX {
				allBackendsToAutoLoad[i] = LLamaCPPAVX
			} else {
				allBackendsToAutoLoad[i] = LLamaCPPFallback
			}
			log.Info().Msgf("Backend: %s", allBackendsToAutoLoad[i])
		}
	}

	if o.model != "" {
		log.Info().Msgf("Trying to load the model '%s' with all the available backends: %s", o.model, strings.Join(allBackendsToAutoLoad, ", "))
	}

	for _, b := range allBackendsToAutoLoad {
		log.Info().Msgf("[%s] Attempting to load", b)
		options := []Option{
			WithBackendString(b),
			WithModel(o.model),
			WithLoadGRPCLoadModelOpts(o.gRPCOptions),
			WithThreads(o.threads),
			WithAssetDir(o.assetDir),
		}

		for k, v := range o.externalBackends {
			options = append(options, WithExternalBackend(k, v))
		}

		model, modelerr := ml.BackendLoader(options...)
		if modelerr == nil && model != nil {
			log.Info().Msgf("[%s] Loads OK", b)
			return model, nil
		} else if modelerr != nil {
			err = errors.Join(err, modelerr)
			log.Info().Msgf("[%s] Fails: %s", b, modelerr.Error())
		} else if model == nil {
			err = errors.Join(err, fmt.Errorf("backend returned no usable model"))
			log.Info().Msgf("[%s] Fails: %s", b, "backend returned no usable model")
		}
	}

	return nil, fmt.Errorf("could not load model - all backends returned error: %s", err.Error())
}
