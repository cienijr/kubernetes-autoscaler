/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	kubeletapis "k8s.io/kubernetes/pkg/kubelet/apis"
)

// resetAWSRegion resets AWS_REGION environment variable key to its pre-test
// value, but only if it was originally present among environment variables.
func resetAWSRegion(value string, present bool) {
	os.Unsetenv("AWS_REGION")
	if present {
		os.Setenv("AWS_REGION", value)
	}
}

// TestGetRegion ensures correct source supplies AWS Region.
func TestGetRegion(t *testing.T) {
	key := "AWS_REGION"
	defer resetAWSRegion(os.LookupEnv(key))
	// Ensure environment variable retains precedence.
	expected1 := "the-shire-1"
	os.Setenv(key, expected1)
	assert.Equal(t, expected1, getRegion())
	// Ensure without environment variable, EC2 Metadata used... and it merely
	// chops the last character off the Availability Zone.
	expected2 := "mordor-2"
	expected2a := expected2 + "a"
	os.Unsetenv(key)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(expected2a))
	}))
	cfg := aws.NewConfig().WithEndpoint(server.URL)
	assert.Equal(t, expected2, getRegion(cfg))
}

func TestBuildGenericLabels(t *testing.T) {
	labels := buildGenericLabels(&asgTemplate{
		InstanceType: &instanceType{
			InstanceType: "c4.large",
			VCPU:         2,
			MemoryMb:     3840,
		},
		Region: "us-east-1",
	}, "sillyname")
	assert.Equal(t, "us-east-1", labels[kubeletapis.LabelZoneRegion])
	assert.Equal(t, "sillyname", labels[kubeletapis.LabelHostname])
	assert.Equal(t, "c4.large", labels[kubeletapis.LabelInstanceType])
	assert.Equal(t, cloudprovider.DefaultArch, labels[kubeletapis.LabelArch])
	assert.Equal(t, cloudprovider.DefaultOS, labels[kubeletapis.LabelOS])
}

func TestExtractLabelsFromAsg(t *testing.T) {
	tags := []*autoscaling.TagDescription{
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/label/foo"),
			Value: aws.String("bar"),
		},
		{
			Key:   aws.String("bar"),
			Value: aws.String("baz"),
		},
	}

	labels := extractLabelsFromAsg(tags)

	assert.Equal(t, 1, len(labels))
	assert.Equal(t, "bar", labels["foo"])
}

func TestExtractTaintsFromAsg(t *testing.T) {
	tags := []*autoscaling.TagDescription{
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/taint/dedicated"),
			Value: aws.String("foo:NoSchedule"),
		},
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/taint/group"),
			Value: aws.String("bar:NoExecute"),
		},
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/taint/app"),
			Value: aws.String("fizz:PreferNoSchedule"),
		},
		{
			Key:   aws.String("bar"),
			Value: aws.String("baz"),
		},
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/taint/blank"),
			Value: aws.String(""),
		},
		{
			Key:   aws.String("k8s.io/cluster-autoscaler/node-template/taint/nosplit"),
			Value: aws.String("some_value"),
		},
	}

	expectedTaints := []apiv1.Taint{
		{
			Key:    "dedicated",
			Value:  "foo",
			Effect: apiv1.TaintEffectNoSchedule,
		},
		{
			Key:    "group",
			Value:  "bar",
			Effect: apiv1.TaintEffectNoExecute,
		},
		{
			Key:    "app",
			Value:  "fizz",
			Effect: apiv1.TaintEffectPreferNoSchedule,
		},
	}

	taints := extractTaintsFromAsg(tags)
	assert.Equal(t, 3, len(taints))
	assert.Equal(t, makeTaintSet(expectedTaints), makeTaintSet(taints))
}

func makeTaintSet(taints []apiv1.Taint) map[apiv1.Taint]bool {
	set := make(map[apiv1.Taint]bool)
	for _, taint := range taints {
		set[taint] = true
	}
	return set
}

func TestFetchExplicitAsgs(t *testing.T) {
	min, max, groupname := 1, 10, "coolasg"

	s := &AutoScalingMock{}
	s.On("DescribeAutoScalingGroups", &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{aws.String(groupname)},
		MaxRecords:            aws.Int64(1),
	}).Return(&autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []*autoscaling.Group{
			{AutoScalingGroupName: aws.String(groupname)},
		},
	})

	s.On("DescribeAutoScalingGroupsPages",
		&autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: aws.StringSlice([]string{groupname}),
			MaxRecords:            aws.Int64(maxRecordsReturnedByAPI),
		},
		mock.AnythingOfType("func(*autoscaling.DescribeAutoScalingGroupsOutput, bool) bool"),
	).Run(func(args mock.Arguments) {
		fn := args.Get(1).(func(*autoscaling.DescribeAutoScalingGroupsOutput, bool) bool)
		zone := "test-1a"
		fn(&autoscaling.DescribeAutoScalingGroupsOutput{
			AutoScalingGroups: []*autoscaling.Group{
				{
					AvailabilityZones:    []*string{&zone},
					AutoScalingGroupName: aws.String(groupname),
					MinSize:              aws.Int64(int64(min)),
					MaxSize:              aws.Int64(int64(max)),
					DesiredCapacity:      aws.Int64(int64(min)),
				},
			}}, false)
	}).Return(nil)

	do := cloudprovider.NodeGroupDiscoveryOptions{
		// Register the same node group twice with different max nodes.
		// The intention is to test that the asgs.Register method will update
		// the node group instead of registering it twice.
		NodeGroupSpecs: []string{
			fmt.Sprintf("%d:%d:%s", min, max, groupname),
			fmt.Sprintf("%d:%d:%s", min, max-1, groupname),
		},
	}
	// #1449 Without AWS_REGION getRegion() lookup runs till timeout during tests.
	defer resetAWSRegion(os.LookupEnv("AWS_REGION"))
	os.Setenv("AWS_REGION", "fanghorn")
	// fetchExplicitASGs is called at manager creation time.
	m, err := createAWSManagerInternal(nil, do, &autoScalingWrapper{s, map[string]string{}}, nil)
	assert.NoError(t, err)

	asgs := m.asgCache.Get()
	assert.Equal(t, 1, len(asgs))
	validateAsg(t, asgs[0], groupname, min, max)
}

func TestBuildInstanceType(t *testing.T) {
	ltName, ltVersion, instanceType := "launcher", "1", "t2.large"

	s := &EC2Mock{}
	s.On("DescribeLaunchTemplateVersions", &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String(ltName),
		Versions:           []*string{aws.String(ltVersion)},
	}).Return(&ec2.DescribeLaunchTemplateVersionsOutput{
		LaunchTemplateVersions: []*ec2.LaunchTemplateVersion{
			{
				LaunchTemplateData: &ec2.ResponseLaunchTemplateData{
					InstanceType: aws.String(instanceType),
				},
			},
		},
	})

	// #1449 Without AWS_REGION getRegion() lookup runs till timeout during tests.
	defer resetAWSRegion(os.LookupEnv("AWS_REGION"))
	os.Setenv("AWS_REGION", "fanghorn")
	m, err := createAWSManagerInternal(nil, cloudprovider.NodeGroupDiscoveryOptions{}, nil, &ec2Wrapper{s})
	assert.NoError(t, err)

	asg := asg{
		LaunchTemplateName:    ltName,
		LaunchTemplateVersion: ltVersion,
	}

	builtInstanceType, err := m.buildInstanceType(&asg)

	assert.NoError(t, err)
	assert.Equal(t, instanceType, builtInstanceType)
}

func TestFetchAutoAsgs(t *testing.T) {
	min, max := 1, 10
	groupname, tags := "coolasg", []string{"tag", "anothertag"}

	s := &AutoScalingMock{}
	// Lookup groups associated with tags
	expectedTagsInput := &autoscaling.DescribeTagsInput{
		Filters: []*autoscaling.Filter{
			{Name: aws.String("key"), Values: aws.StringSlice([]string{tags[0]})},
			{Name: aws.String("key"), Values: aws.StringSlice([]string{tags[1]})},
		},
		MaxRecords: aws.Int64(maxRecordsReturnedByAPI),
	}
	// Use MatchedBy pattern to avoid list order issue https://github.com/kubernetes/autoscaler/issues/1346
	s.On("DescribeTagsPages", mock.MatchedBy(tagsMatcher(expectedTagsInput)),
		mock.AnythingOfType("func(*autoscaling.DescribeTagsOutput, bool) bool"),
	).Run(func(args mock.Arguments) {
		fn := args.Get(1).(func(*autoscaling.DescribeTagsOutput, bool) bool)
		fn(&autoscaling.DescribeTagsOutput{
			Tags: []*autoscaling.TagDescription{
				{ResourceId: aws.String(groupname)},
				{ResourceId: aws.String(groupname)},
			}}, false)
	}).Return(nil).Once()

	// Describe the group to register it, then again to generate the instance
	// cache.
	s.On("DescribeAutoScalingGroupsPages",
		&autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: aws.StringSlice([]string{groupname}),
			MaxRecords:            aws.Int64(maxRecordsReturnedByAPI),
		},
		mock.AnythingOfType("func(*autoscaling.DescribeAutoScalingGroupsOutput, bool) bool"),
	).Run(func(args mock.Arguments) {
		fn := args.Get(1).(func(*autoscaling.DescribeAutoScalingGroupsOutput, bool) bool)
		zone := "test-1a"
		fn(&autoscaling.DescribeAutoScalingGroupsOutput{
			AutoScalingGroups: []*autoscaling.Group{{
				AvailabilityZones:    []*string{&zone},
				AutoScalingGroupName: aws.String(groupname),
				MinSize:              aws.Int64(int64(min)),
				MaxSize:              aws.Int64(int64(max)),
				DesiredCapacity:      aws.Int64(int64(min)),
			}}}, false)
	}).Return(nil).Twice()

	do := cloudprovider.NodeGroupDiscoveryOptions{
		NodeGroupAutoDiscoverySpecs: []string{fmt.Sprintf("asg:tag=%s", strings.Join(tags, ","))},
	}

	// #1449 Without AWS_REGION getRegion() lookup runs till timeout during tests.
	defer resetAWSRegion(os.LookupEnv("AWS_REGION"))
	os.Setenv("AWS_REGION", "fanghorn")
	// fetchAutoASGs is called at manager creation time, via forceRefresh
	m, err := createAWSManagerInternal(nil, do, &autoScalingWrapper{s, map[string]string{}}, nil)
	assert.NoError(t, err)

	asgs := m.asgCache.Get()
	assert.Equal(t, 1, len(asgs))
	validateAsg(t, asgs[0], groupname, min, max)

	// Simulate the previously discovered ASG disappearing
	s.On("DescribeTagsPages", mock.MatchedBy(tagsMatcher(expectedTagsInput)),
		mock.AnythingOfType("func(*autoscaling.DescribeTagsOutput, bool) bool"),
	).Run(func(args mock.Arguments) {
		fn := args.Get(1).(func(*autoscaling.DescribeTagsOutput, bool) bool)
		fn(&autoscaling.DescribeTagsOutput{Tags: []*autoscaling.TagDescription{}}, false)
	}).Return(nil).Once()

	err = m.asgCache.regenerate()
	assert.NoError(t, err)
	assert.Empty(t, m.asgCache.Get())
}

func tagsMatcher(expected *autoscaling.DescribeTagsInput) func(*autoscaling.DescribeTagsInput) bool {
	return func(actual *autoscaling.DescribeTagsInput) bool {
		expectedTags := flatTagSlice(expected.Filters)
		actualTags := flatTagSlice(actual.Filters)

		return *expected.MaxRecords == *actual.MaxRecords && reflect.DeepEqual(expectedTags, actualTags)
	}
}

func flatTagSlice(filters []*autoscaling.Filter) []string {
	tags := []string{}
	for _, filter := range filters {
		tags = append(tags, aws.StringValueSlice(filter.Values)...)
	}
	// Sort slice for compare
	sort.Strings(tags)
	return tags
}
