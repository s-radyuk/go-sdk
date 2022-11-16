package statsig

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type configSpec struct {
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	Salt              string          `json:"salt"`
	Enabled           bool            `json:"enabled"`
	Rules             []configRule    `json:"rules"`
	DefaultValue      json.RawMessage `json:"defaultValue"`
	IDType            string          `json:"idType"`
	ExplicitParamters []string        `json:"explicitParameters"`
}

type configRule struct {
	Name           string            `json:"name"`
	ID             string            `json:"id"`
	Salt           string            `json:"salt"`
	PassPercentage float64           `json:"passPercentage"`
	Conditions     []configCondition `json:"conditions"`
	ReturnValue    json.RawMessage   `json:"returnValue"`
	IDType         string            `json:"idType"`
	ConfigDelegate string            `json:"configDelegate"`
}

type configCondition struct {
	Type             string                 `json:"type"`
	Operator         string                 `json:"operator"`
	Field            string                 `json:"field"`
	TargetValue      interface{}            `json:"targetValue"`
	AdditionalValues map[string]interface{} `json:"additionalValues"`
	IDType           string                 `json:"idType"`
}

type downloadConfigSpecResponse struct {
	HasUpdates     bool            `json:"has_updates"`
	Time           int64           `json:"time"`
	FeatureGates   []configSpec    `json:"feature_gates"`
	DynamicConfigs []configSpec    `json:"dynamic_configs"`
	LayerConfigs   []configSpec    `json:"layer_configs"`
	IDLists        map[string]bool `json:"id_lists"`
}

type downloadConfigsInput struct {
	SinceTime       int64           `json:"sinceTime"`
	StatsigMetadata statsigMetadata `json:"statsigMetadata"`
}

type idList struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	CreationTime int64  `json:"creationTime"`
	URL          string `json:"url"`
	FileID       string `json:"fileID"`
	ids          *sync.Map
}

type getIDListsInput struct {
	StatsigMetadata statsigMetadata `json:"statsigMetadata"`
}

type store struct {
	featureGates         map[string]configSpec
	dynamicConfigs       map[string]configSpec
	layerConfigs         map[string]configSpec
	configsLock          sync.RWMutex
	idLists              map[string]*idList
	idListsLock          sync.RWMutex
	lastSyncTime         int64
	lastSyncTimeLock     sync.RWMutex
	initialSyncTime      int64
	initialSyncTimeLock  sync.RWMutex
	initReason           evaluationReason
	initReasonLock       sync.RWMutex
	transport            *transport
	configSyncInterval   time.Duration
	idListSyncInterval   time.Duration
	shutdown             bool
	shutdownLock         sync.Mutex
	rulesUpdatedCallback func(rules string, time int64)
	errorBoundary        *errorBoundary
}

func newStore(
	transport *transport,
	errorBoundary *errorBoundary,
	options *Options,
) *store {
	configSyncInterval := 10 * time.Second
	idListSyncInterval := time.Minute
	if options.ConfigSyncInterval > 0 {
		configSyncInterval = options.ConfigSyncInterval
	}
	if options.IDListSyncInterval > 0 {
		idListSyncInterval = options.IDListSyncInterval
	}
	return newStoreInternal(
		transport,
		configSyncInterval,
		idListSyncInterval,
		options.BootstrapValues,
		options.RulesUpdatedCallback,
		errorBoundary,
	)
}

func newStoreInternal(
	transport *transport,
	configSyncInterval time.Duration,
	idListSyncInterval time.Duration,
	bootstrapValues string,
	rulesUpdatedCallback func(rules string, time int64),
	errorBoundary *errorBoundary,
) *store {
	store := &store{
		featureGates:         make(map[string]configSpec),
		dynamicConfigs:       make(map[string]configSpec),
		idLists:              make(map[string]*idList),
		transport:            transport,
		configSyncInterval:   configSyncInterval,
		idListSyncInterval:   idListSyncInterval,
		rulesUpdatedCallback: rulesUpdatedCallback,
		errorBoundary:        errorBoundary,
		initReason:           reasonUninitialized,
	}
	if bootstrapValues != "" {
		specs := downloadConfigSpecResponse{}
		err := json.Unmarshal([]byte(bootstrapValues), &specs)
		if err == nil {
			store.setConfigSpecs(specs)
			store.initReasonLock.Lock()
			store.initReason = reasonBootstrap
			store.initReasonLock.Unlock()
		}
	}
	store.fetchConfigSpecs()
	store.lastSyncTimeLock.RLock()
	store.initialSyncTimeLock.Lock()
	store.initialSyncTime = store.lastSyncTime
	store.lastSyncTimeLock.RUnlock()
	store.initialSyncTimeLock.Unlock()
	store.syncIDLists()
	go store.pollForRulesetChanges()
	go store.pollForIDListChanges()
	return store
}

func (s *store) getGate(name string) (configSpec, bool) {
	s.configsLock.RLock()
	defer s.configsLock.RUnlock()
	gate, ok := s.featureGates[name]
	return gate, ok
}

func (s *store) getDynamicConfig(name string) (configSpec, bool) {
	s.configsLock.RLock()
	defer s.configsLock.RUnlock()
	config, ok := s.dynamicConfigs[name]
	return config, ok
}

func (s *store) getLayerConfig(name string) (configSpec, bool) {
	s.configsLock.RLock()
	defer s.configsLock.RUnlock()
	config, ok := s.layerConfigs[name]
	return config, ok
}

func (s *store) fetchConfigSpecs() {
	s.lastSyncTimeLock.RLock()
	input := &downloadConfigsInput{
		SinceTime:       s.lastSyncTime,
		StatsigMetadata: s.transport.metadata,
	}
	s.lastSyncTimeLock.RUnlock()
	var specs downloadConfigSpecResponse
	err := s.transport.postRequest("/download_config_specs", input, &specs)
	if err != nil {
		s.errorBoundary.logException(err)
		return
	}
	if s.setConfigSpecs(specs) && s.rulesUpdatedCallback != nil {
		v, _ := json.Marshal(specs)
		s.rulesUpdatedCallback(string(v[:]), specs.Time)
	}
}

func (s *store) setConfigSpecs(specs downloadConfigSpecResponse) bool {
	if specs.HasUpdates {
		// TODO: when adding eval details, differentiate REASON between bootstrap and network here
		newGates := make(map[string]configSpec)
		for _, gate := range specs.FeatureGates {
			newGates[gate.Name] = gate
		}

		newConfigs := make(map[string]configSpec)
		for _, config := range specs.DynamicConfigs {
			newConfigs[config.Name] = config
		}

		newLayers := make(map[string]configSpec)
		for _, layer := range specs.LayerConfigs {
			newLayers[layer.Name] = layer
		}

		s.configsLock.Lock()
		s.featureGates = newGates
		s.dynamicConfigs = newConfigs
		s.layerConfigs = newLayers
		s.configsLock.Unlock()
		s.lastSyncTimeLock.Lock()
		s.lastSyncTime = specs.Time
		s.lastSyncTimeLock.Unlock()
		s.initReasonLock.Lock()
		s.initReason = reasonNetwork
		s.initReasonLock.Unlock()
		return true
	}
	return false
}

func (s *store) getIDList(name string) *idList {
	s.idListsLock.RLock()
	defer s.idListsLock.RUnlock()
	list, ok := s.idLists[name]
	if ok {
		return list
	}
	return nil
}

func (s *store) deleteIDList(name string) {
	s.idListsLock.Lock()
	defer s.idListsLock.Unlock()
	delete(s.idLists, name)
}

func (s *store) setIDList(name string, list *idList) {
	s.idListsLock.Lock()
	defer s.idListsLock.Unlock()
	s.idLists[name] = list
}

func (s *store) syncIDLists() {
	var serverLists map[string]idList
	err := s.transport.postRequest("/get_id_lists", getIDListsInput{StatsigMetadata: s.transport.metadata}, &serverLists)
	if err != nil {
		s.errorBoundary.logException(err)
		return
	}

	wg := sync.WaitGroup{}
	for name, serverList := range serverLists {
		localList := s.getIDList(name)
		if localList == nil {
			localList = &idList{Name: name}
			s.setIDList(name, localList)
		}

		// skip if server list is invalid
		if serverList.URL == "" || serverList.CreationTime < localList.CreationTime || serverList.FileID == "" {
			continue
		}

		// reset the local list if returns server list has a newer file
		if serverList.FileID != localList.FileID && serverList.CreationTime >= localList.CreationTime {
			localList = &idList{
				Name:         localList.Name,
				Size:         0,
				CreationTime: serverList.CreationTime,
				URL:          serverList.URL,
				FileID:       serverList.FileID,
				ids:          &sync.Map{},
			}
			s.setIDList(name, localList)
		}

		// skip if server list is not bigger
		if serverList.Size <= localList.Size {
			continue
		}

		wg.Add(1)
		go func(name string, l *idList) {
			defer wg.Done()
			res, err := s.transport.get(l.URL, map[string]string{"Range": fmt.Sprintf("bytes=%d-", l.Size)})
			if err != nil || res == nil {
				s.errorBoundary.logException(err)
				return
			}
			defer res.Body.Close()

			length, err := strconv.Atoi(res.Header.Get("content-length"))
			if err != nil || length <= 0 {
				s.errorBoundary.logException(err)
				return
			}

			bodyBytes, err := io.ReadAll(res.Body)
			if err != nil {
				s.errorBoundary.logException(err)
				return
			}
			content := string(bodyBytes)
			if len(content) <= 1 || (string(content[0]) != "-" && string(content[0]) != "+") {
				s.deleteIDList(name)
				return
			}

			lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if len(line) <= 1 {
					continue
				}
				id := line[1:]
				op := string(line[0])
				if op == "+" {
					l.ids.Store(id, true)
				} else if op == "-" {
					l.ids.Delete(id)
				}
			}
			atomic.AddInt64((&l.Size), int64(length))
		}(name, localList)
	}
	wg.Wait()
	for name := range s.idLists {
		if _, ok := serverLists[name]; !ok {
			s.deleteIDList(name)
		}
	}
}

func (s *store) pollForIDListChanges() {
	for {
		time.Sleep(s.idListSyncInterval)
		stop := func() bool {
			s.shutdownLock.Lock()
			defer s.shutdownLock.Unlock()
			return s.shutdown
		}()
		if stop {
			break
		}
		s.syncIDLists()
	}
}

func (s *store) pollForRulesetChanges() {
	for {
		time.Sleep(s.configSyncInterval)
		stop := func() bool {
			s.shutdownLock.Lock()
			defer s.shutdownLock.Unlock()
			return s.shutdown
		}()
		if stop {
			break
		}
		s.fetchConfigSpecs()
	}
}

func (s *store) stopPolling() {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()
	s.shutdown = true
}
