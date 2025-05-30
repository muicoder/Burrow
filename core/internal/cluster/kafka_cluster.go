// Copyright 2017 LinkedIn Corp. Licensed under the Apache License, Version
// 2.0 (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.

package cluster

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/linkedin/Burrow/core/internal/helpers"
	"github.com/linkedin/Burrow/core/internal/httpserver"
	"github.com/linkedin/Burrow/core/protocol"
)

// KafkaCluster is a cluster module which connects to a single Apache Kafka cluster and manages the broker topic and
// partition information. It periodically updates a list of all topics and partitions, and also fetches the broker
// end offset (latest) for each partition. This information is forwarded to the storage module for use in consumer
// evaluations.
type KafkaCluster struct {
	// App is a pointer to the application context. This stores the channel to the storage subsystem
	App *protocol.ApplicationContext

	// Log is a logger that has been configured for this module to use. Normally, this means it has been set up with
	// fields that are appropriate to identify this coordinator
	Log *zap.Logger

	name                string
	saramaConfig        *sarama.Config
	servers             []string
	offsetRefresh       int
	topicRefresh        int
	groupsReaperRefresh int

	offsetTicker       *time.Ticker
	metadataTicker     *time.Ticker
	groupsReaperTicker *time.Ticker
	quitChannel        chan struct{}
	running            sync.WaitGroup

	fetchMetadata   bool
	topicPartitions map[string][]int32
}

// Configure validates the configuration for the cluster. At minimum, there must be a list of servers provided for the
// Kafka cluster, of the form host:port. Default values will be set for the intervals to use for refreshing offsets
// (10 seconds) and topics (60 seconds). A missing, or bad, list of servers will cause this func to panic.
func (module *KafkaCluster) Configure(name, configRoot string) {
	module.Log.Info("configuring")

	module.name = name
	module.quitChannel = make(chan struct{})
	module.running = sync.WaitGroup{}

	profile := viper.GetString(configRoot + ".client-profile")
	module.saramaConfig = helpers.GetSaramaConfigFromClientProfile(profile)

	module.servers = viper.GetStringSlice(configRoot + ".servers")
	if len(module.servers) == 0 {
		panic("No Kafka brokers specified for cluster " + module.name)
	} else if !helpers.ValidateHostList(module.servers) {
		panic("Cluster '" + name + "' has one or more improperly formatted servers (must be host:port)")
	}

	// Set defaults for configs if needed
	viper.SetDefault(configRoot+".offset-refresh", 10)
	viper.SetDefault(configRoot+".topic-refresh", 60)
	viper.SetDefault(configRoot+".groups-reaper-refresh", 0)
	module.offsetRefresh = viper.GetInt(configRoot + ".offset-refresh")
	module.topicRefresh = viper.GetInt(configRoot + ".topic-refresh")
	module.groupsReaperRefresh = viper.GetInt(configRoot + ".groups-reaper-refresh")
}

// Start connects to the Kafka cluster using the Shopify/sarama client. Any error connecting to the cluster is returned
// to the caller. Once the client is set up, tickers are started to periodically refresh topics and offsets.
func (module *KafkaCluster) Start() error {
	module.Log.Info("starting")

	// Connect Kafka client
	client, err := sarama.NewClient(module.servers, module.saramaConfig)
	if err != nil {
		module.Log.Error("failed to start client[cluster]version:"+module.saramaConfig.Version.String(), zap.Error(err))
	}
	if os.Getenv("CLUSTERS_VERSION") == "" {
		vers := len(sarama.SupportedVersions)
		for index := range vers {
			module.saramaConfig.Version = sarama.SupportedVersions[vers-index-1]
			if client, err = sarama.NewClient(module.servers, module.saramaConfig); err == nil {
				module.Log.Info("try using client[cluster]version:" + module.saramaConfig.Version.String())
				break
			}
		}
	}

	// Fire off the offset requests once, before we start the ticker, to make sure we start with good data for consumers
	helperClient := &helpers.BurrowSaramaClient{
		Client: client,
	}
	module.fetchMetadata = true
	module.getOffsets(helperClient)

	// Start main loop that has a timer for offset and topic fetches
	module.offsetTicker = time.NewTicker(time.Duration(module.offsetRefresh) * time.Second)
	module.metadataTicker = time.NewTicker(time.Duration(module.topicRefresh) * time.Second)

	if module.groupsReaperRefresh != 0 {
		module.groupsReaperTicker = time.NewTicker(time.Duration(module.groupsReaperRefresh) * time.Second)
		if !module.saramaConfig.Version.IsAtLeast(sarama.V0_11_0_0) {
			module.groupsReaperTicker.Stop()
			module.Log.Warn("groups reaper disabled, it needs at least kafka v0.11.0.0 to get the list of consumer groups")
		}
	} else {
		// just start and stop a new ticker, the channel will still be active but will not emit ticks
		// it'll simplify tick management in the mainLoop func
		module.groupsReaperTicker = time.NewTicker(1 * time.Minute)
		module.groupsReaperTicker.Stop()
	}
	go module.mainLoop(helperClient)

	return nil
}

// Stop causes both the topic and offset refresh tickers to be stopped, and then it closes the Kafka client.
func (module *KafkaCluster) Stop() error {
	module.Log.Info("stopping")

	module.metadataTicker.Stop()
	module.offsetTicker.Stop()
	module.groupsReaperTicker.Stop()
	close(module.quitChannel)
	module.running.Wait()

	return nil
}

func (module *KafkaCluster) mainLoop(client helpers.SaramaClient) {
	module.running.Add(1)
	defer module.running.Done()

	for {
		select {
		case <-module.offsetTicker.C:
			module.getOffsets(client)
		case <-module.metadataTicker.C:
			// Update metadata on next offset fetch
			module.fetchMetadata = true
		case <-module.groupsReaperTicker.C:
			module.reapNonExistingGroups(client)
		case <-module.quitChannel:
			return
		}
	}
}

func (module *KafkaCluster) maybeUpdateMetadataAndDeleteTopics(client helpers.SaramaClient) {
	if module.fetchMetadata {
		module.fetchMetadata = false
		client.RefreshMetadata()

		// Get the current list of topics and make a map
		topicList, err := client.Topics()
		if err != nil {
			module.Log.Error("failed to fetch topic list", zap.String("sarama_error", err.Error()))
			return
		}

		// We'll use topicPartitions later
		topicPartitions := make(map[string][]int32)
		for _, topic := range topicList {
			partitions, err := client.Partitions(topic)
			if err != nil {
				module.Log.Error("failed to fetch partition list", zap.String("sarama_error", err.Error()))
				return
			}

			topicPartitions[topic] = make([]int32, 0, len(partitions))
			for _, partitionID := range partitions {
				if _, err := client.Leader(topic, partitionID); err != nil {
					module.Log.Warn("failed to fetch leader for partition",
						zap.String("topic", topic),
						zap.Int32("partition", partitionID),
						zap.String("sarama_error", err.Error()))
				} else { // partitionID has a leader
					// NOTE: append only happens here
					// so cap(topicPartitions[topic]) is the partition count
					topicPartitions[topic] = append(topicPartitions[topic], partitionID)
				}
			}
		}

		// Check for deleted topics if we have a previous map to check against
		if module.topicPartitions != nil {
			for topic := range module.topicPartitions {
				if _, ok := topicPartitions[topic]; !ok {
					// Topic no longer exists - tell storage to delete it
					module.App.StorageChannel <- &protocol.StorageRequest{
						RequestType: protocol.StorageSetDeleteTopic,
						Cluster:     module.name,
						Topic:       topic,
					}
					httpserver.DeleteTopicMetrics(module.name, topic)
				}
			}
		}

		// Save the new topicPartitions for next time
		module.topicPartitions = topicPartitions
	}
}

func (module *KafkaCluster) generateOffsetRequests(client helpers.SaramaClient) (map[int32]*sarama.OffsetRequest, map[int32]helpers.SaramaBroker) {
	requests := make(map[int32]*sarama.OffsetRequest)
	brokers := make(map[int32]helpers.SaramaBroker)

	// Generate an OffsetRequest for each topic:partition and bucket it to the leader broker
	for topic, partitions := range module.topicPartitions {
		for _, partitionID := range partitions {
			broker, err := client.Leader(topic, partitionID)
			if err != nil {
				module.Log.Warn("failed to fetch leader for partition",
					zap.String("topic", topic),
					zap.Int32("partition", partitionID),
					zap.String("sarama_error", err.Error()))
				module.fetchMetadata = true
				continue
			}
			if _, ok := requests[broker.ID()]; !ok {
				requests[broker.ID()] = &sarama.OffsetRequest{}
				// Match the version of the client as sarama's getOffset function does
				// https://github.com/IBM/sarama/blob/main/client.go#L863-L876
				if client.Config().Version.IsAtLeast(sarama.V2_1_0_0) {
					// Version 4 adds the current leader epoch, which is used for fencing.
					requests[broker.ID()].Version = 4
				} else if client.Config().Version.IsAtLeast(sarama.V2_0_0_0) {
					// Version 3 is the same as version 2.
					requests[broker.ID()].Version = 3
				} else if client.Config().Version.IsAtLeast(sarama.V0_11_0_0) {
					// Version 2 adds the isolation level, which is used for transactional reads.
					requests[broker.ID()].Version = 2
				} else if client.Config().Version.IsAtLeast(sarama.V0_10_1_0) {
					// Version 1 removes MaxNumOffsets.  From this version forward, only a single
					// offset can be returned.
					requests[broker.ID()].Version = 1
				}
			}
			brokers[broker.ID()] = broker
			requests[broker.ID()].AddBlock(topic, partitionID, sarama.OffsetNewest, 1)
		}
	}

	return requests, brokers
}

// This function performs massively parallel OffsetRequests, which is better than Sarama's internal implementation,
// which does one at a time. Several orders of magnitude faster.
func (module *KafkaCluster) getOffsets(client helpers.SaramaClient) {
	module.maybeUpdateMetadataAndDeleteTopics(client)
	requests, brokers := module.generateOffsetRequests(client)

	// Send out the OffsetRequest to each broker for all the partitions it is leader for
	// The results go to the offset storage module
	var wg = sync.WaitGroup{}
	var errorTopics = sync.Map{}

	getBrokerOffsets := func(brokerID int32, request *sarama.OffsetRequest) {
		defer wg.Done()
		response, err := brokers[brokerID].GetAvailableOffsets(request)
		if err != nil {
			module.Log.Error("failed to fetch offsets from broker",
				zap.String("sarama_error", err.Error()),
				zap.Int32("broker", brokerID),
			)
			brokers[brokerID].Close()
			return
		}
		ts := time.Now().Unix() * 1000
		for topic, partitions := range response.Blocks {
			for partition, offsetResponse := range partitions {
				if offsetResponse.Err != sarama.ErrNoError {
					module.Log.Warn("error in OffsetResponse",
						zap.String("sarama_error", offsetResponse.Err.Error()),
						zap.Int32("broker", brokerID),
						zap.String("topic", topic),
						zap.Int32("partition", partition),
					)

					// Gather a list of topics that had errors
					errorTopics.Store(topic, true)
					continue
				}
				offset := &protocol.StorageRequest{
					RequestType:         protocol.StorageSetBrokerOffset,
					Cluster:             module.name,
					Topic:               topic,
					Partition:           partition,
					Offset:              offsetResponse.Offsets[0],
					Timestamp:           ts,
					TopicPartitionCount: int32(cap(module.topicPartitions[topic])),
				}
				helpers.TimeoutSendStorageRequest(module.App.StorageChannel, offset, 1)
			}
		}
	}

	for brokerID, request := range requests {
		wg.Add(1)
		go getBrokerOffsets(brokerID, request)
	}

	wg.Wait()

	// If there are any topics that had errors, force a metadata refresh on the next run
	errorTopics.Range(func(key, value interface{}) bool {
		module.fetchMetadata = true
		return false
	})
}

func (module *KafkaCluster) reapNonExistingGroups(client helpers.SaramaClient) {
	kafkaGroups, err := client.ListConsumerGroups()
	if err != nil {
		module.Log.Error("failed to get the list of available consumer groups", zap.Error(err))
		return
	}

	req := &protocol.StorageRequest{
		RequestType: protocol.StorageFetchConsumers,
		Reply:       make(chan interface{}),
		Cluster:     module.name,
	}
	helpers.TimeoutSendStorageRequest(module.App.StorageChannel, req, 20)

	res := <-req.Reply
	if res == nil {
		module.Log.Warn("groups reaper: couldn't get list of consumer groups from storage")
		return
	}

	// TODO: find how to get reportedConsumerGroup from KafkaClient
	burrowIgnoreGroupName := "burrow-" + module.name
	burrowGroups, _ := res.([]string)
	for _, g := range burrowGroups {
		if g == burrowIgnoreGroupName {
			continue
		}
		if _, ok := kafkaGroups[g]; !ok {
			module.Log.Info(fmt.Sprintf("groups reaper: removing non existing kafka consumer group (%s) from burrow", g))
			request := &protocol.StorageRequest{
				RequestType: protocol.StorageSetDeleteGroup,
				Cluster:     module.name,
				Group:       g,
			}
			helpers.TimeoutSendStorageRequest(module.App.StorageChannel, request, 1)

			httpserver.DeleteConsumerMetrics(module.name, g)
		}
	}
}
