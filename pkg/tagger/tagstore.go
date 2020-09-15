package tagger

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/DataDog/datadog-agent/pkg/tagger/collectors"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// entityTags holds the tag information for a given entity
type entityTags struct {
	sync.RWMutex
	lowCardTags          map[string][]string
	orchestratorCardTags map[string][]string
	highCardTags         map[string][]string
	standardTags         map[string][]string
	cacheValid           bool
	cachedSource         []string
	cachedAll            []string // Low + orchestrator + high
	cachedOrchestrator   []string // Low + orchestrator (subslice of cachedAll)
	cachedLow            []string // Sub-slice of cachedAll
	tagsHash             string
}

// tagStore stores entity tags in memory and handles search and collation.
// Queries should go through the Tagger for cache-miss handling
type tagStore struct {
	storeMutex sync.RWMutex
	store      map[string]*entityTags

	toDeleteMutex sync.RWMutex
	toDelete      map[string]struct{} // set emulation

	subscribersMutex sync.RWMutex
	subscribers      map[chan EntityEvent]collectors.TagCardinality
}

func newTagStore() *tagStore {
	return &tagStore{
		store:       make(map[string]*entityTags),
		toDelete:    make(map[string]struct{}),
		subscribers: make(map[chan EntityEvent]collectors.TagCardinality),
	}
}

func (s *tagStore) processTagInfo(info *collectors.TagInfo) error {
	if info == nil {
		return fmt.Errorf("skipping nil message")
	}
	if info.Entity == "" {
		return fmt.Errorf("empty entity name, skipping message")
	}
	if info.Source == "" {
		return fmt.Errorf("empty source name, skipping message")
	}
	if info.DeleteEntity {
		s.toDeleteMutex.Lock()
		s.toDelete[info.Entity] = struct{}{}
		s.toDeleteMutex.Unlock()
		return nil
	}

	eventType := EventTypeModified

	// TODO: check if real change
	s.storeMutex.Lock()
	defer s.storeMutex.Unlock()
	storedTags, exist := s.store[info.Entity]
	if !exist {
		storedTags = &entityTags{
			lowCardTags:          make(map[string][]string),
			orchestratorCardTags: make(map[string][]string),
			highCardTags:         make(map[string][]string),
			standardTags:         make(map[string][]string),
		}
		s.store[info.Entity] = storedTags

		eventType = EventTypeAdded

		storedEntities.Inc()
	}

	updatedEntities.Inc()

	err := updateStoredTags(storedTags, info)
	if err != nil {
		return err
	}

	s.notifySubscribers(info.Entity, storedTags, eventType)

	return nil
}

func updateStoredTags(storedTags *entityTags, info *collectors.TagInfo) error {
	storedTags.Lock()
	defer storedTags.Unlock()
	_, found := storedTags.lowCardTags[info.Source]
	if found && info.CacheMiss {
		// check if the source tags is already present for this entry
		// Only check once since we always write all cardinality tag levels.
		err := fmt.Errorf("try to overwrite an existing entry with and empty cache-miss entry, info.Source: %s, info.Entity: %s", info.Source, info.Entity)
		log.Tracef("processTagInfo err: %v", err)
		return err
	}
	storedTags.lowCardTags[info.Source] = info.LowCardTags
	storedTags.orchestratorCardTags[info.Source] = info.OrchestratorCardTags
	storedTags.highCardTags[info.Source] = info.HighCardTags
	storedTags.standardTags[info.Source] = info.StandardTags
	storedTags.cacheValid = false

	return nil
}

// Entity is an entity ID + tags.
type Entity struct {
	ID   string
	Tags []string
}

// EventType is a type of event, triggered when an entity is added, modified or
// deleted.
type EventType int

const (
	// EventTypeAdded means an entity was added.
	EventTypeAdded EventType = iota
	// EventTypeMOodified means an entity was modified.
	EventTypeModified
	// EventTypeDeleted means an entity was deleted.
	EventTypeDeleted
)

// EntityEvent is an event generated when an entity is added, modified or
// deleted. It contains the event type and the new entity.
type EntityEvent struct {
	EventType EventType
	Entity    Entity
}

// subscribe returns a list of existing entities in the store, alongside a
// channel that receives events whenever an entity is added, modified or
// deleted.
func (s *tagStore) subscribe(cardinality collectors.TagCardinality) ([]EntityEvent, chan EntityEvent) {
	// this buffer size is an educated guess, as we know the rate of
	// updates, but not how fast these can be streamed out yet. it most
	// likely should be configurable.
	bufferSize := 100
	ch := make(chan EntityEvent, bufferSize)

	s.storeMutex.RLock()
	defer s.storeMutex.RUnlock()
	events := make([]EntityEvent, 0, len(s.store))
	for entityID, et := range s.store {
		tags, _, _ := et.get(cardinality)

		events = append(events, EntityEvent{
			EventType: EventTypeAdded,
			Entity: Entity{
				ID:   entityID,
				Tags: copyArray(tags),
			},
		})
	}

	s.subscribersMutex.Lock()
	defer s.subscribersMutex.Unlock()
	s.subscribers[ch] = cardinality

	return events, ch
}

// unsubscribe ends a subscription to entity events and closes its channel.
func (s *tagStore) unsubscribe(ch chan EntityEvent) {
	s.subscribersMutex.Lock()
	defer s.subscribersMutex.Unlock()

	delete(s.subscribers, ch)
	close(ch)
}

// notifySubscribers sends an EntityEvent for an entity to all registered
// subscribers.
func (s *tagStore) notifySubscribers(id string, storedTags *entityTags, eventType EventType) {
	s.subscribersMutex.RLock()
	defer s.subscribersMutex.RUnlock()

	for ch, cardinality := range s.subscribers {
		var tags []string

		if storedTags != nil {
			tags, _, _ = storedTags.get(cardinality)
		}

		ch <- EntityEvent{
			EventType: eventType,
			Entity: Entity{
				ID:   id,
				Tags: tags,
			},
		}
	}
}

func computeTagsHash(tags []string) string {
	hash := ""
	if len(tags) > 0 {
		// do not sort original slice
		tags = copyArray(tags)
		h := fnv.New64()
		sort.Strings(tags)
		for _, i := range tags {
			h.Write([]byte(i)) //nolint:errcheck
		}
		hash = strconv.FormatUint(h.Sum64(), 16)
	}
	return hash
}

// prune will lock the store and delete tags for the entity previously
// passed as delete. This is to be called regularly from the user class.
func (s *tagStore) prune() error {
	s.toDeleteMutex.Lock()
	defer s.toDeleteMutex.Unlock()

	if len(s.toDelete) == 0 {
		return nil
	}

	s.storeMutex.Lock()
	defer s.storeMutex.Unlock()
	for entity := range s.toDelete {
		delete(s.store, entity)
		s.notifySubscribers(entity, nil, EventTypeDeleted)
	}

	remainingEntities := len(s.store)
	log.Debugf("pruned %d removed entities, %d remaining", len(s.toDelete), remainingEntities)

	// Start fresh
	s.toDelete = make(map[string]struct{})

	storedEntities.Set(float64(remainingEntities))

	return nil
}

// lookup gets tags from the store and returns them concatenated in a string
// slice. It returns the source names in the second slice to allow the
// client to trigger manual lookups on missing sources, the last string
// is the tags hash to have a snapshot digest of all the tags.
func (s *tagStore) lookup(entity string, cardinality collectors.TagCardinality) ([]string, []string, string) {
	s.storeMutex.RLock()
	defer s.storeMutex.RUnlock()
	storedTags, present := s.store[entity]

	if present == false {
		return nil, nil, ""
	}
	return storedTags.get(cardinality)
}

// lookupStandard returns the standard tags recorded for a given entity
func (s *tagStore) lookupStandard(entity string) ([]string, error) {
	s.storeMutex.RLock()
	defer s.storeMutex.RUnlock()
	storedTags, present := s.store[entity]
	if present == false {
		return nil, fmt.Errorf("entity %s not found", entity)
	}
	return storedTags.getStandard(), nil
}

func (e *entityTags) getStandard() []string {
	e.RLock()
	defer e.RUnlock()
	tags := []string{}
	for _, t := range e.standardTags {
		tags = append(tags, t...)
	}
	return tags
}

type tagPriority struct {
	tag         string                       // full tag
	priority    collectors.CollectorPriority // collector priority
	cardinality collectors.TagCardinality    // cardinality level of the tag (low, orchestrator, high)
}

func (e *entityTags) get(cardinality collectors.TagCardinality) ([]string, []string, string) {
	e.Lock()
	defer e.Unlock()

	// Cache hit
	if e.cacheValid {
		if cardinality == collectors.HighCardinality {
			return e.cachedAll, e.cachedSource, e.tagsHash
		} else if cardinality == collectors.OrchestratorCardinality {
			return e.cachedOrchestrator, e.cachedSource, e.tagsHash
		}
		return e.cachedLow, e.cachedSource, e.tagsHash
	}

	// Cache miss
	var sources []string
	tagPrioMapper := make(map[string][]tagPriority)

	for source, tags := range e.lowCardTags {
		sources = append(sources, source)
		insertWithPriority(tagPrioMapper, tags, source, collectors.LowCardinality)
	}

	for source, tags := range e.orchestratorCardTags {
		insertWithPriority(tagPrioMapper, tags, source, collectors.OrchestratorCardinality)
	}

	for source, tags := range e.highCardTags {
		insertWithPriority(tagPrioMapper, tags, source, collectors.HighCardinality)
	}

	var lowCardTags []string
	var orchestratorCardTags []string
	var highCardTags []string
	for _, tags := range tagPrioMapper {
		for i := 0; i < len(tags); i++ {
			insert := true
			for j := 0; j < len(tags); j++ {
				// if we find a duplicate tag with higher priority we do not insert the tag
				if i != j && tags[i].priority < tags[j].priority {
					insert = false
					break
				}
			}
			if !insert {
				continue
			}
			if tags[i].cardinality == collectors.HighCardinality {
				highCardTags = append(highCardTags, tags[i].tag)
				continue
			} else if tags[i].cardinality == collectors.OrchestratorCardinality {
				orchestratorCardTags = append(orchestratorCardTags, tags[i].tag)
				continue
			}
			lowCardTags = append(lowCardTags, tags[i].tag)
		}
	}

	tags := append(lowCardTags, orchestratorCardTags...)
	tags = append(tags, highCardTags...)

	// Write cache
	e.cacheValid = true
	e.cachedSource = sources
	e.cachedAll = tags
	e.cachedLow = e.cachedAll[:len(lowCardTags)]
	e.cachedOrchestrator = e.cachedAll[:len(lowCardTags)+len(orchestratorCardTags)]
	e.tagsHash = computeTagsHash(e.cachedAll)

	if cardinality == collectors.HighCardinality {
		return tags, sources, e.tagsHash
	} else if cardinality == collectors.OrchestratorCardinality {
		return e.cachedOrchestrator, sources, e.tagsHash
	}
	return lowCardTags, sources, e.tagsHash
}

func insertWithPriority(tagPrioMapper map[string][]tagPriority, tags []string, source string, cardinality collectors.TagCardinality) {
	priority, found := collectors.CollectorPriorities[source]
	if !found {
		log.Warnf("Tagger: %s collector has no defined priority, assuming low", source)
		priority = collectors.NodeRuntime
	}

	for _, t := range tags {
		tagName := strings.Split(t, ":")[0]
		tagPrioMapper[tagName] = append(tagPrioMapper[tagName], tagPriority{
			tag:         t,
			priority:    priority,
			cardinality: cardinality,
		})
	}
}
