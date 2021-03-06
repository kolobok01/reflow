// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package ec2cluster

//go:generate go run ../cmd/ec2instances/main.go instances

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/grailbio/base/data"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/config"
	"github.com/grailbio/reflow/ec2cluster/instances"
	"github.com/grailbio/reflow/errors"
	"github.com/grailbio/reflow/internal/ecrauth"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/pool"
	"github.com/grailbio/reflow/pool/client"
	yaml "gopkg.in/yaml.v2"
)

// memoryDiscount is the amount of memory that's reserved
// by the reflowlet.
const memoryDiscount = 0.05

var ec2UserDataTmpl = template.Must(template.New("ec2userdata").Parse(ec2UserData))

const ec2UserData = `#cloud-config
write_files:
  - path: "/etc/ecrlogin"
    permissions: "0644"
    owner: "root"
    content: |
      {{.LoginCommand}}

  - path: "/etc/reflowconfig"
    permissions: "0644"
    owner: "root"
    content: |
      {{.ReflowConfig}}

coreos:
  update:
    reboot-strategy: "off"

  units:
  - name: update-engine.service
    command: stop

  - name: locksmithd.service
    command: stop

  - name: format-{{.DeviceName}}.service
    command: start
    content: |
      [Unit]
      Description=Format /dev/{{.DeviceName}}
      After=dev-{{.DeviceName}}.device
      Requires=dev-{{.DeviceName}}.device
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/sbin/wipefs -f /dev/{{.DeviceName}}
      ExecStart=/usr/sbin/mkfs.ext4 -F /dev/{{.DeviceName}}

  - name: mnt-data.mount
    command: start
    content: |
      [Mount]
      What=/dev/{{.DeviceName}}
      Where=/mnt/data
      Type=ext4
      Options=data=writeback

  - name: reflowlet.service
    enable: true
    command: start
    content: |
      [Unit]
      Description=reflowlet
      Requires=network.target
      After=network.target
{{if .Mortal}}
      OnFailure=poweroff.target
      OnFailureJobMode=replace-irreversibly
{{end}}
      
      [Service]
      Type=oneshot
      ExecStartPre=-/usr/bin/docker stop %n
      ExecStartPre=-/usr/bin/docker rm %n
      ExecStartPre=-/bin/bash -c 'sleep $[( $RANDOM % {{.Count}} ) ]'
      ExecStartPre=/bin/bash /etc/ecrlogin
      ExecStartPre=/usr/bin/docker pull {{.ReflowletImage}}
      ExecStart=/usr/bin/docker run --rm --name %n --net=host \
        -v /:/host \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -v '/etc/ssl/certs/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt' \
        {{.ReflowletImage}} -prefix /host -ec2cluster -ndigest 60 -config /host/etc/reflowconfig
      
      [Install]
      WantedBy=multi-user.target

  - name: "node-exporter.service"
    enable: true
    command: "start"
    content: |
      [Unit]
      Description=node-exporter
      Requires=network.target
      After=network.target
      After=mnt-data.mount
      [Service]
      Restart=always
      TimeoutStartSec=infinity
      RestartSec=10s
      StartLimitInterval=0
      ExecStartPre=-/usr/bin/docker stop %n
      ExecStartPre=-/usr/bin/docker rm %n
      ExecStartPre=/usr/bin/docker pull prom/node-exporter:0.12.0
      ExecStart=/usr/bin/docker run --rm --name %n -p 9100:9100 -v /proc:/host/proc -v /sys:/host/sys -v /:/rootfs --net=host prom/node-exporter:0.12.0 -collector.procfs /host/proc -collector.sysfs /host/proc -collector.filesystem.ignored-mount-points "^/(sys|proc|dev|host|etc)($|/)"
      [Install]
      WantedBy=multi-user.target

ssh-authorized-keys:
  - {{.SshKey}}
`

// instanceConfig represents a instance configuration.
type instanceConfig struct {
	// Type is the EC2 instance type to be launched.
	Type string

	// EBSOptimized is true if we should request an EBS optimized instance.
	EBSOptimized bool
	// Resources holds the Reflow resources that are presented by this configuration.
	// It does not include disk sizes; they are dynamic.
	Resources reflow.Resources
	// Price is the on-demand price for this instance type in fractional dollars, in available regions.
	Price map[string]float64
	// SpotOk tells whether spot is supported for this instance type.
	SpotOk bool
	// NVMe specifies whether EBS is exposed as NVMe devices.
	NVMe bool
}

var (
	instanceTypes     = map[string]instanceConfig{}
	instanceTypesOnce sync.Once
)

func init() {
	for _, typ := range instances.Types {
		instanceTypes[typ.Name] = instanceConfig{
			Type:         typ.Name,
			EBSOptimized: typ.EBSOptimized,
			Price:        typ.Price,
			Resources: reflow.Resources{
				CPU:    uint16(typ.VCPU),
				Memory: uint64((1 - memoryDiscount) * typ.Memory * 1024 * 1024 * 1024),
			},
			// According to Amazon, "t2" instances are the only current-generation
			// instances not supported by spot.
			SpotOk: typ.Generation == "current" && !strings.HasPrefix(typ.Name, "t2."),
			NVMe:   typ.NVMe,
		}
	}
}

// instanceState stores everything we know about EC2 instances,
// and implements instance type selection according to runtime
// criteria.
type instanceState struct {
	configs   []instanceConfig
	sleepTime time.Duration
	region    string

	mu          sync.Mutex
	unavailable map[string]time.Time
}

func newInstanceState(configs []instanceConfig, sleep time.Duration, region string) *instanceState {
	s := &instanceState{
		configs:     make([]instanceConfig, len(configs)),
		unavailable: make(map[string]time.Time),
		sleepTime:   sleep,
		region:      region,
	}
	copy(s.configs, configs)
	sort.Slice(s.configs, func(i, j int) bool {
		return s.configs[j].Resources.Memory < s.configs[i].Resources.Memory
	})
	return s
}

// Unavailable marks the given instance config as busy.
func (s *instanceState) Unavailable(config instanceConfig) {
	s.mu.Lock()
	s.unavailable[config.Type] = time.Now()
	s.mu.Unlock()
}

// Max returns the maximum instance config that could
// ever be available.
func (s *instanceState) Max() instanceConfig {
	return s.configs[0]
}

// MaxAvailable returns the maximum instance config currently
// believed to be available. Spot restricts instances to those that
// may be launched via EC2 spot market.
func (s *instanceState) MaxAvailable(spot bool) (instanceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, config := range s.configs {
		if time.Since(s.unavailable[config.Type]) < s.sleepTime || (spot && !config.SpotOk) {
			continue
		}
		return config, true
	}
	return instanceConfig{}, false
}

// MinAvailable returns the cheapest instance type that has at least
// the required resources and is also believed to be currently
// available. Spot restricts instances to those that may be launched
// via EC2 spot market.
func (s *instanceState) MinAvailable(need reflow.Resources, spot bool) (instanceConfig, bool) {
	best, ok := s.MaxAvailable(spot)
	if !ok {
		return instanceConfig{}, ok
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, candidate := range s.configs {
		if time.Since(s.unavailable[candidate.Type]) < s.sleepTime {
			continue
		}
		price := candidate.Price[s.region]
		if price == 0 {
			continue
		}
		if (!spot || candidate.SpotOk) && need.LessEqualAll(candidate.Resources) && price < best.Price[s.region] {
			best = candidate
		}
	}
	return best, true
}

func (s *instanceState) Type(typ string) (instanceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.unavailable[typ]) < s.sleepTime {
		return instanceConfig{}, false
	}
	for _, config := range s.configs {
		if config.Type == typ {
			return config, true
		}
	}
	return instanceConfig{}, false
}

// instance represents a concrete instance; it is launched from an instanceConfig
// and additional parameters.
type instance struct {
	HTTPClient      *http.Client
	Config          instanceConfig
	ReflowConfig    config.Config
	Log             *log.Logger
	Authenticator   ecrauth.Interface
	EC2             *ec2.EC2
	Tag             string
	Labels          pool.Labels
	Spot            bool
	InstanceProfile string
	SecurityGroup   string
	Region          string
	ReflowletImage  string
	Price           float64
	EBSType         string
	EBSSize         uint64
	AMI             string
	KeyName         string
	SshKey          string

	userData string
	err      error
	ec2inst  *ec2.Instance
}

// Err returns any error that occured while launching the instance.
func (i *instance) Err() error {
	return i.err
}

// Instance returns the EC2 instance metadata returned by a successful launch.
func (i *instance) Instance() *ec2.Instance {
	return i.ec2inst
}

// Go launches an instance, and returns when it fails or the context is done.
// On success (i.Err() == nil), the returned instance is in running state.
func (i *instance) Go(ctx context.Context) {
	const maxTries = 5
	type stateT int
	const (
		// Perform capacity check for EC2 spot.
		stateCapacity stateT = iota
		// Launch the instance via EC2.
		stateLaunch
		// Tag the instance
		stateTag
		// Wait for the instance to enter running state.
		stateWait
		// Describe the instance via EC2.
		stateDescribe
		// Wait for offers to appear--i.e., the Reflowlet is live.
		stateOffers
		stateDone
	)
	var (
		state stateT
		id    string
		dns   string
		n     int
		d     = 5 * time.Second
	)
	// TODO(marius): propagate context to the underlying AWS calls
	for state < stateDone && ctx.Err() == nil {
		switch state {
		case stateCapacity:
			if !i.Spot {
				break
			}
			// 20 instances should be a good margin for spot.
			var ok bool
			ok, i.err = i.ec2HasCapacity(ctx, 20)
			if i.err == nil && !ok {
				i.err = errors.E(errors.Unavailable, errors.New("ec2 capacity is likely exhausted"))
			}
		case stateLaunch:
			id, i.err = i.launch(ctx)
			if i.err != nil {
				i.Log.Errorf("instance launch error: %v", i.err)
			} else {
				spot := ""
				if i.Spot {
					spot = "spot "
				}
				i.Log.Printf("launched %sinstance %v: %s: %s %d %s",
					spot,
					id, i.Config.Type,
					data.Size(i.Config.Resources.Memory),
					i.Config.Resources.CPU,
					data.Size(i.Config.Resources.Disk))
			}

		case stateTag:
			input := &ec2.CreateTagsInput{
				Resources: []*string{aws.String(id)},
				Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String(i.Tag)}},
			}
			for k, v := range i.Labels {
				input.Tags = append(input.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
			}
			_, i.err = i.EC2.CreateTags(input)
		case stateWait:
			i.err = i.EC2.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(id)},
			})
		case stateDescribe:
			var resp *ec2.DescribeInstancesOutput
			resp, i.err = i.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(id)},
			})
			if len(resp.Reservations) != 1 || len(resp.Reservations[0].Instances) != 1 {
				i.err = errors.Errorf("ec2.describeinstances %v: invalid output", id)
			}
			if i.err == nil {
				i.ec2inst = resp.Reservations[0].Instances[0]
				if i.ec2inst.PublicDnsName == nil || *i.ec2inst.PublicDnsName == "" {
					i.err = errors.Errorf("ec2.describeinstances %v: no public DNS name", id)
				} else {
					dns = *i.ec2inst.PublicDnsName
				}
			}
		case stateOffers:
			var pool pool.Pool
			pool, i.err = client.New(fmt.Sprintf("https://%s:9000/v1/", dns), i.HTTPClient, nil /*log.New(os.Stderr, "client: ", 0)*/)
			if i.err != nil {
				i.err = errors.E(errors.Fatal, i.err)
				break
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, i.err = pool.Offers(ctx)
			if i.err != nil && strings.HasSuffix(i.err.Error(), "connection refused") {
				i.err = errors.E(errors.Temporary, i.err)
			}
			cancel()
		default:
			panic("unknown state")
		}
		if i.err == nil {
			n = 0
			d = 5 * time.Second
			state++
			continue
		}
		if n == maxTries {
			break
		}
		if awserr, ok := i.err.(awserr.Error); ok {
			switch awserr.Code() {
			// According to EC2 API docs, these codes indicate
			// capacity issues.
			//
			// http://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html
			//
			// TODO(marius): add a separate package for interpreting AWS errors.
			case "InsufficientCapacity", "InsufficientInstanceCapacity", "InsufficientHostCapacity", "InsufficientReservedInstanceCapacity", "InstanceLimitExceeded":
				i.err = errors.E(errors.Unavailable, awserr)
			}
		}
		switch {
		case i.err == nil:
		case errors.Match(errors.Fatal, i.err):
			return
		case errors.Match(errors.Unavailable, i.err):
			// Return these immediately because our caller may be able to handle
			// them by selecting a different instance type.
			return
		case !errors.Recover(i.err).Timeout() && !errors.Recover(i.err).Temporary():
			i.Log.Errorf("instance error: %v", i.err)
		}
		time.Sleep(d)
		n++
		d *= time.Duration(2)
	}
	if i.err != nil {
		return
	}
	i.err = ctx.Err()
}

func (i *instance) launch(ctx context.Context) (string, error) {
	args := struct {
		Count          int
		LoginCommand   string
		Mortal         bool
		ReflowConfig   string
		ReflowletImage string
		SshKey         string
		DeviceName     string
	}{}
	args.Count = 1
	args.Mortal = true

	keys := make(config.Keys)
	if err := i.ReflowConfig.Marshal(keys); err != nil {
		return "", err
	}
	// The remote side does not need a cluster implementation.
	delete(keys, config.Cluster)
	b, err := yaml.Marshal(keys)
	if err != nil {
		return "", err
	}
	args.ReflowConfig = string(b)
	// This ugly hack is required to properly embed the (YAML) configuration
	// inside another YAML file.
	args.ReflowConfig = strings.Replace(args.ReflowConfig, "\n", "\n      ", -1)
	args.LoginCommand, err = ecrauth.Login(context.TODO(), i.Authenticator)
	if err != nil {
		return "", err
	}
	args.ReflowletImage = i.ReflowletImage
	args.SshKey = i.SshKey
	if args.SshKey == "" {
		i.Log.Debugf("instance launch: missing public SSH key")
	}
	args.DeviceName = "xvdb"
	if i.Config.NVMe {
		args.DeviceName = "nvme1n1"
	}

	var userdataBuf bytes.Buffer
	if err := ec2UserDataTmpl.Execute(&userdataBuf, args); err != nil {
		return "", err
	}
	i.userData = base64.StdEncoding.EncodeToString(userdataBuf.Bytes())
	if i.Spot {
		return i.ec2RunSpotInstance(ctx)
	}
	return i.ec2RunInstance()
}

func (i *instance) ec2RunSpotInstance(ctx context.Context) (string, error) {
	i.Log.Debugf("generating ec2 spot instance request for instance type %v", i.Config.Type)
	// First make a spot instance request.
	params := &ec2.RequestSpotInstancesInput{
		ValidUntil: aws.Time(time.Now().Add(time.Minute)),
		SpotPrice:  aws.String(fmt.Sprintf("%.3f", i.Price)),

		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			ImageId:      aws.String(i.AMI),
			EbsOptimized: aws.Bool(i.Config.EBSOptimized),
			InstanceType: aws.String(i.Config.Type),

			BlockDeviceMappings: []*ec2.BlockDeviceMapping{
				{
					// The root device for the OS, Docker images, etc.
					DeviceName: aws.String("/dev/xvda"),
					Ebs: &ec2.EbsBlockDevice{
						DeleteOnTermination: aws.Bool(true),
						VolumeSize:          aws.Int64(200),
						VolumeType:          aws.String("gp2"),
					},
				},
				{
					// The data device used for all Reflow data.
					DeviceName: aws.String("/dev/xvdb"),
					Ebs: &ec2.EbsBlockDevice{
						DeleteOnTermination: aws.Bool(true),
						VolumeSize:          aws.Int64(int64(i.EBSSize)),
						VolumeType:          aws.String(i.EBSType),
					},
				},
			},

			KeyName:  nonemptyString(i.KeyName),
			UserData: aws.String(i.userData),

			SecurityGroupIds: []*string{aws.String(i.SecurityGroup)},
		},
	}
	resp, err := i.EC2.RequestSpotInstances(params)
	if err != nil {
		return "", err
	}
	if n := len(resp.SpotInstanceRequests); n != 1 {
		return "", errors.Errorf("ec2.requestspotinstances: got %v entries, want 1", n)
	}
	reqid := aws.StringValue(resp.SpotInstanceRequests[0].SpotInstanceRequestId)
	if reqid == "" {
		return "", errors.Errorf("ec2.requestspotinstances: empty request id")
	}
	i.Log.Debugf("waiting for spot fullfillment for instance type %v: %s", i.Config.Type, reqid)
	// Also set a timeout context in case the AWS API is stuck.
	toctx, cancel := context.WithTimeout(ctx, time.Minute+10*time.Second)
	defer cancel()
	if err := i.ec2WaitForSpotFulfillment(toctx, reqid); err != nil {
		// If we're not fulfilled by our deadline, we consider spot instances
		// unavailable. Boot this up to the caller so they can pick a different
		// instance types.
		return "", errors.E(errors.Unavailable, err)
	}
	describe, err := i.EC2.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String(reqid)},
	})
	if err != nil {
		return "", err
	}
	if n := len(describe.SpotInstanceRequests); n != 1 {
		return "", errors.Errorf("ec2.describespotinstancerequests: got %v entries, want 1", n)
	}
	id := aws.StringValue(describe.SpotInstanceRequests[0].InstanceId)
	if id == "" {
		return "", errors.Errorf("ec2.describespotinstancerequests: missing instance ID")
	}
	i.Log.Debugf("ec2 spot request %s fulfilled", reqid)
	return id, nil
}

// ec2WaitForSpotFulfillment waits until the spot request spotID has been fulfilled.
// It differs from (*ec2.EC2).WaitUntilSpotInstanceRequestFulfilledWithContext
// in that it request-cancelled-and-instance-running as a success.
func (i *instance) ec2WaitForSpotFulfillment(ctx context.Context, spotID string) error {
	w := request.Waiter{
		Name:        "ec2WaitForSpotFulfillment",
		MaxAttempts: 40,                                            // default from SDK
		Delay:       request.ConstantWaiterDelay(15 * time.Second), // default from SDK
		Acceptors: []request.WaiterAcceptor{
			{
				State:   request.SuccessWaiterState,
				Matcher: request.PathAllWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "fulfilled",
			},
			{
				State:   request.SuccessWaiterState,
				Matcher: request.PathAllWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "request-canceled-and-instance-running",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "schedule-expired",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "canceled-before-fulfillment",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "bad-parameters",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "system-error",
			},
		},
		NewRequest: func(opts []request.Option) (*request.Request, error) {
			req, _ := i.EC2.DescribeSpotInstanceRequestsRequest(&ec2.DescribeSpotInstanceRequestsInput{
				SpotInstanceRequestIds: []*string{aws.String(spotID)},
			})
			req.SetContext(ctx)
			req.ApplyOptions(opts...)
			return req, nil
		},
	}
	return w.WaitWithContext(ctx)
}

func (i *instance) ec2HasCapacity(ctx context.Context, n int) (bool, error) {
	params := &ec2.RunInstancesInput{
		DryRun:       aws.Bool(true),
		MinCount:     aws.Int64(int64(n)),
		MaxCount:     aws.Int64(int64(n)),
		ImageId:      aws.String(i.AMI),
		InstanceType: aws.String(i.Config.Type),
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := i.EC2.RunInstancesWithContext(ctx, params)
	if err == nil {
		return false, errors.New("did not expect succesful response")
	} else if awserr, ok := err.(awserr.Error); ok {
		if awserr.Code() == "DryRunOperation" {
			return true, nil
		}
		return false, awserr
	} else if err == context.DeadlineExceeded {
		// We'll take an API timeout as a negative answer: this seems to
		// the case empirically.
		return false, nil
	} else if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("expected awserr.Error or context error, got %T", err)
}

func (i *instance) ec2RunInstance() (string, error) {
	params := &ec2.RunInstancesInput{
		ImageId:  aws.String(i.AMI),
		MaxCount: aws.Int64(int64(1)),
		MinCount: aws.Int64(int64(1)),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				// The root device for the OS, Docker images, etc.
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &ec2.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(true),
					VolumeSize:          aws.Int64(200),
					VolumeType:          aws.String("gp2"),
				},
			},
			{
				// The data device used for all Reflow data.
				DeviceName: aws.String("/dev/xvdb"),
				Ebs: &ec2.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(true),
					VolumeSize:          aws.Int64(int64(i.EBSSize)),
					VolumeType:          aws.String(i.EBSType),
				},
			},
		},
		ClientToken:           aws.String(newID()),
		DisableApiTermination: aws.Bool(false),
		DryRun:                aws.Bool(false),
		EbsOptimized:          aws.Bool(i.Config.EBSOptimized),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Arn: aws.String(i.InstanceProfile),
		},
		InstanceInitiatedShutdownBehavior: aws.String("terminate"),
		InstanceType:                      aws.String(i.Config.Type),
		Monitoring: &ec2.RunInstancesMonitoringEnabled{
			Enabled: aws.Bool(true), // Required
		},
		KeyName:          nonemptyString(i.KeyName),
		UserData:         aws.String(i.userData),
		SecurityGroupIds: []*string{aws.String(i.SecurityGroup)},
	}
	resv, err := i.EC2.RunInstances(params)
	if err != nil {
		return "", err
	}
	if n := len(resv.Instances); n != 1 {
		return "", fmt.Errorf("expected 1 instance; got %d", n)
	}
	return *resv.Instances[0].InstanceId, nil
}

func newID() string {
	var b [8]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b[:])
}

// nonemptyString returns nil if s is empty, or else the pointer to s.
func nonemptyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
