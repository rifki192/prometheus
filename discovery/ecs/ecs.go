// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ecs

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/client_golang/prometheus"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/util/strutil"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/auth/credentials"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
)

// Consts for all ecsLabel
const (
	ecsLabel               = model.MetaLabelPrefix + "ecs_"
	ecsLabelAZ             = ecsLabel + "availability_zone"
	ecsLabelInstanceID     = ecsLabel + "instance_id"
	ecsLabelInstanceStatus = ecsLabel + "instance_status"
	ecsLabelInstanceType   = ecsLabel + "instance_type"
	ecsLabelElasticIP      = ecsLabel + "elastic_ip"
	ecsLabelPublicIP       = ecsLabel + "public_ip"
	ecsLabelPrivateIP      = ecsLabel + "private_ip"
	ecsLabelSubnetID       = ecsLabel + "subnet_id"
	ecsLabelTag            = ecsLabel + "tag_"
	ecsLabelVPCID          = ecsLabel + "vpc_id"
	subnetSeparator        = ","
	pageSize               = 100
)

var (
	ecsSDRefreshFailureCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "prometheus_sd_ecs_refresh_failures_total",
			Help: "The number of ECS-SD scrape failures",
		})
	ecsSDRefreshDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name: "prometheus_sd_ecs_refresh_duration_seconds",
			Help: "The duration of a ECS-SD refresh in seconds",
		})
	// DefaultSDConfig is the default EC2 SD configuration.
	DefaultSDConfig = SDConfig{
		Port:            9100,
		RefreshInterval: model.Duration(60 * time.Second),
	}
)

// Configuration for filtering ECS instances
type Filter struct {
	Name   string   `yaml:"name"`
	Values []string `yaml:"values"`
}

type SDConfig struct {
	Region          []string           `yaml:"region"`
	AccessKey       string             `yaml:"access_key,omitempty"`
	SecretKey       config_util.Secret `yaml:"secret_key,omitempty"`
	RefreshInterval model.Duration     `yaml:"refresh_interval,omitempty"`
	Port            int                `yaml:"port"`
	Filters         []*Filter          `yaml:"filters"`
}

func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if c.Region == nil {
		return fmt.Errorf("ECS SD configuration requires a region")
	}
	return nil
}

func init() {
	prometheus.MustRegister(ecsSDRefreshFailureCount)
	prometheus.MustRegister(ecsSDRefreshDuration)
}

type Discovery struct {
	config   SDConfig
	interval time.Duration
	port     int
	filters  []*Filter
	logger   log.Logger
}

func NewDiscovery(conf *SDConfig, logger log.Logger) *Discovery {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &Discovery{
		config:   *conf,
		interval: time.Duration(conf.RefreshInterval),
		port:     conf.Port,
		filters:  conf.Filters,
		logger:   logger,
	}
}

func (d *Discovery) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	// Get an initial set right away.
	tg, err := d.refresh()
	if err != nil {
		level.Error(d.logger).Log("msg", "Refresh failed", "err", err)
	} else {
		select {
		case ch <- []*targetgroup.Group{tg}:
		case <-ctx.Done():
			return
		}
	}

	for {
		select {
		case <-ticker.C:
			tg, err := d.refresh()
			if err != nil {
				level.Error(d.logger).Log("msg", "Refresh failed", "err", err)
				continue
			}

			select {
			case ch <- []*targetgroup.Group{tg}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *Discovery) refresh() (tg *targetgroup.Group, err error) {
	t0 := time.Now()
	defer func() {
		ecsSDRefreshDuration.Observe(time.Since(t0).Seconds())
		if err != nil {
			ecsSDRefreshFailureCount.Inc()
		}
	}()

	config := sdk.NewConfig()
	// Create a credential object
	credential := &credentials.BaseCredential{
		AccessKeyId:     d.config.AccessKey,
		AccessKeySecret: string(d.config.SecretKey),
	}
	// Initiate the client
	client, err := ecs.NewClientWithOptions(d.config.Region, config, credential)
	if err != nil {
		return nil, fmt.Errorf("could not create alibaba client: %s", err)
	}

	tg = &targetgroup.Group{
		Source: d.config.Region,
	}

	/// Create an API request and set parameters
	request := ecs.CreateDescribeInstancesRequest()
	// Set the request.PageSize
	request.PageSize = requests.NewInteger(pageSize)
	totalPages := 1
	for i := 1; i <= totalPages; i++ {
		request.PageNumber = requests.NewInteger(int(i))
		response, err := client.DescribeInstances(request)
		if err != nil {
			// Handle exceptions
			return nil, fmt.Errorf("could not send request to alibaba: %s", err)
		}
		totalInstances := response.TotalCount
		totalPages = int(math.Ceil(float64(totalInstances) / float64(pageSize)))

		for _, inst := range response.Instances.Instance {
			labels := model.LabelSet{
				ecsLabelInstanceID: model.LabelValue(inst.InstanceId),
			}
			labels[ecsLabelPrivateIP] = model.LabelValue(inst.NetworkInterfaces.NetworkInterface[0].PrimaryIpAddress)
			addr := net.JoinHostPort(inst.NetworkInterfaces.NetworkInterface[0].PrimaryIpAddress, fmt.Sprintf("%d", d.port))
			labels[model.AddressLabel] = model.LabelValue(addr)
			if len(inst.PublicIpAddress.IpAddress) > 0 {
				labels[ecsLabelPublicIP] = model.LabelValue(inst.PublicIpAddress.IpAddress[0])
			}
			if inst.EipAddress.IpAddress != "" {
				labels[ecsLabelElasticIP] = model.LabelValue(inst.EipAddress.IpAddress)
			}

			labels[ecsLabelAZ] = model.LabelValue(inst.ZoneId)
			labels[ecsLabelInstanceType] = model.LabelValue(inst.InstanceType)
			labels[ecsLabelInstanceStatus] = model.LabelValue(inst.Status)

			if inst.VpcAttributes.VpcId != "" {
				labels[ecsLabelVPCID] = model.LabelValue(inst.VpcAttributes.VpcId)

				subnetsMap := make(map[string]struct{})
				for _, eni := range inst.NetworkInterfaces.NetworkInterface {
					subnetsMap[eni.VSwitchId] = struct{}{}
				}
				subnets := []string{}
				for k := range subnetsMap {
					subnets = append(subnets, k)
				}
				labels[ecsLabelSubnetID] = model.LabelValue(
					subnetSeparator +
						strings.Join(subnets, subnetSeparator) +
						subnetSeparator)
			}

			for _, tag := range inst.Tags.Tag {
				if tag.TagKey == "" || tag.TagValue == "" {
					continue
				}
				name := strutil.SanitizeLabelName(tag.TagKey)
				labels[ecsLabelTag+model.LabelName(name)] = model.LabelValue(tag.TagValue)
			}
			tg.Targets = append(tg.Targets, labels)
		}
	}

	return tg, nil
}
