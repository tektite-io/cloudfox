package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BishopFox/cloudfox/internal"
	"github.com/BishopFox/cloudfox/internal/aws/policy"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/bishopfox/awsservicemap"
	"github.com/sirupsen/logrus"
)

type SQSModule struct {
	// General configuration data
	SQSClient AWSSQSClient

	StorePolicies bool

	Caller       sts.GetCallerIdentityOutput
	AWSRegions   []string
	OutputFormat string
	Goroutines   int
	AWSProfile   string
	WrapTable    bool

	// Main module data
	Queues         []Queue
	CommandCounter internal.CommandCounter
	// Used to store output data for pretty printing
	output internal.OutputData2
	modLog *logrus.Entry
}

type AWSSQSClient interface {
	ListQueues(ctx context.Context, params *sqs.ListQueuesInput, optFns ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

type Queue struct {
	URL                   string
	Name                  string
	Region                string
	Policy                policy.Policy
	PolicyJSON            string
	Access                string
	IsPublic              string
	IsConditionallyPublic string
	Statement             string
	Actions               string
	ConditionText         string
	ResourcePolicySummary string
}

func (m *SQSModule) PrintSQS(outputFormat string, outputDirectory string, verbosity int) {
	// These stuct values are used by the output module
	m.output.Verbosity = verbosity
	m.output.Directory = outputDirectory
	m.output.CallingModule = "sqs"
	m.modLog = internal.TxtLog.WithFields(logrus.Fields{
		"module": m.output.CallingModule,
	})
	m.output.FilePath = filepath.Join(outputDirectory, "cloudfox-output", "aws", m.AWSProfile)
	if m.AWSProfile == "" {
		m.AWSProfile = internal.BuildAWSPath(m.Caller)
	}

	fmt.Printf("[%s][%s] Enumerating SQS queues for account %s.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), aws.ToString(m.Caller.Account))

	wg := new(sync.WaitGroup)
	semaphore := make(chan struct{}, m.Goroutines)

	// Create a channel to signal the spinner aka task status goroutine to finish
	spinnerDone := make(chan bool)
	//fire up the the task status spinner/updated
	go internal.SpinUntil(m.output.CallingModule, &m.CommandCounter, spinnerDone, "regions")

	//create a channel to receive the objects
	dataReceiver := make(chan Queue)

	// Create a channel to signal to stop
	receiverDone := make(chan bool)
	go m.Receiver(dataReceiver, receiverDone)

	for _, region := range m.AWSRegions {
		wg.Add(1)
		m.CommandCounter.Pending++
		go m.executeChecks(region, wg, semaphore, dataReceiver)

	}

	wg.Wait()
	// Send a message to the spinner goroutine to close the channel and stop
	spinnerDone <- true
	<-spinnerDone
	// Send a message to the data receiver goroutine to close the channel and stop
	receiverDone <- true
	<-receiverDone

	// add - if struct is not empty do this. otherwise, dont write anything.
	m.output.Headers = []string{
		// "URL",
		"Name",
		"Region",
		"Public?",
		//"Stmt",
		//"Who?",
		//"Cond. Public",
		//"Can do what?",
		//"Conditions?",
		"Resource Policy Summary",
	}

	sort.SliceStable(m.Queues, func(i, j int) bool {
		return m.Queues[i].URL < m.Queues[j].URL
	})

	// Table rows
	for i := range m.Queues {
		m.output.Body = append(
			m.output.Body,
			[]string{
				//	 m.Queues[i].URL,
				m.Queues[i].Name,
				m.Queues[i].Region,
				m.Queues[i].IsPublic,
				//m.Queues[i].Statement,
				//m.Queues[i].Access,
				//m.Queues[i].IsConditionallyPublic,
				//m.Queues[i].Actions,
				//m.Queues[i].ConditionText,
				m.Queues[i].ResourcePolicySummary,
			},
		)

	}
	if len(m.output.Body) > 0 {
		//m.output.OutputSelector(outputFormat)
		internal.OutputSelector(verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule, m.WrapTable, m.AWSProfile)
		fmt.Printf("[%s][%s] %s queues found.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), strconv.Itoa(len(m.output.Body)))
		if m.StorePolicies {
			fmt.Printf("[%s][%s] Access policies stored to: %s\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), m.getLootDir())
		}
	} else {
		fmt.Printf("[%s][%s] No queues found, skipping the creation of an output file.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile))
	}

}

func (m *SQSModule) executeChecks(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Queue) {
	defer wg.Done()
	servicemap := &awsservicemap.AwsServiceMap{
		JsonFileSource: "EMBEDDED_IN_PACKAGE",
	}
	res, err := servicemap.IsServiceInRegion("sqs", r)
	if err != nil {
		m.modLog.Error(err)
	}
	if res {
		m.CommandCounter.Total++
		wg.Add(1)
		m.getSQSRecordsPerRegion(r, wg, semaphore, dataReceiver)
	}
}
func (m *SQSModule) Receiver(receiver chan Queue, receiverDone chan bool) {
	defer close(receiverDone)
	for {
		select {
		case data := <-receiver:
			m.Queues = append(m.Queues, data)
		case <-receiverDone:
			receiverDone <- true
			return
		}
	}
}

func (m *SQSModule) getSQSRecordsPerRegion(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Queue) {
	defer func() {
		m.CommandCounter.Executing--
		m.CommandCounter.Complete++
		wg.Done()

	}()
	semaphore <- struct{}{}
	defer func() {
		<-semaphore
	}()
	// "PaginationMarker" is a control variable used for output continuity, as AWS return the output in pages.
	var PaginationControl *string

	for {
		ListQueues, err := m.SQSClient.ListQueues(
			context.TODO(),
			&sqs.ListQueuesInput{
				MaxResults: aws.Int32(1000),
				NextToken:  PaginationControl,
			},
			func(o *sqs.Options) {
				o.Region = r
			},
		)
		if err != nil {
			m.modLog.Error(err.Error())
			m.CommandCounter.Error++
			break
		}

		for _, url := range ListQueues.QueueUrls {
			queue, err := m.getQueueWithAttributes(url, r)
			if err != nil {
				m.modLog.Error(err.Error())
				m.CommandCounter.Error++
				break
			}

			// easier to just set the default state to be no and only flip it to yes if we have a case that matches
			queue.IsPublic = "No"

			if !queue.Policy.IsEmpty() {
				m.analyseQueuePolicy(queue, dataReceiver)
			} else {
				// If the queue policy "resource policy" is empty, the only principals that have permisisons
				// are those that are granted access by IAM policies
				//queue.Access = "Private. Access allowed by IAM policies"
				queue.Access = "Only intra-account access (via IAM) allowed"
				dataReceiver <- *queue

			}

		}

		// The "NextToken" value is nil when there's no more data to return.
		if ListQueues.NextToken != nil {
			PaginationControl = ListQueues.NextToken
		} else {
			PaginationControl = nil
			break
		}
	}
}

func (m *SQSModule) getQueueWithAttributes(queueURL string, region string) (*Queue, error) {
	queue := &Queue{
		URL: queueURL,
	}

	GetQueueAttributes, err := m.SQSClient.GetQueueAttributes(
		context.TODO(),
		&sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(queueURL),
			AttributeNames: []types.QueueAttributeName{
				types.QueueAttributeNamePolicy,
				types.QueueAttributeNameQueueArn,
			},
		},
		func(o *sqs.Options) {
			o.Region = region
		},
	)
	if err != nil {
		return nil, fmt.Errorf("GetQueueAttributes(%s) failed: %s", queueURL, err)
	}

	if queueArn, ok := GetQueueAttributes.Attributes[string(types.QueueAttributeNameQueueArn)]; ok {
		parsedArn, err := arn.Parse(queueArn)
		if err != nil {
			queue.Name = queueArn
		}
		queue.Name = parsedArn.Resource
		queue.Region = parsedArn.Region
	}

	if policyJSON, ok := GetQueueAttributes.Attributes[string(types.QueueAttributeNamePolicy)]; ok {
		policy, err := policy.ParseJSONPolicy([]byte(policyJSON))
		if err != nil {
			return nil, fmt.Errorf("parsing queue access policy (%s) as JSON: %s", queueURL, err)
		}

		queue.PolicyJSON = policyJSON
		queue.Policy = policy
	}

	return queue, nil
}

func (m *SQSModule) analyseQueuePolicy(queue *Queue, dataReceiver chan Queue) {
	if queue.Policy.IsPublic() {
		queue.Access = "Anyone"

		if m.StorePolicies {
			m.storeAccessPolicy("public", queue)
		}

	} else if queue.Policy.IsConditionallyPublic() {

		queue.IsConditionallyPublic = "public-wc"
		queue.Access = "Access restricted by conditions"

		if m.StorePolicies {
			m.storeAccessPolicy("public-wc", queue)
		}
	}

	if queue.Policy.IsPublic() && !queue.Policy.IsConditionallyPublic() {
		queue.IsPublic = "YES"
	}

	for i, statement := range queue.Policy.Statement {
		var prefix string = ""
		if len(queue.Policy.Statement) > 1 {
			prefix = fmt.Sprintf("Statement %d says: ", i)
		}

		//queue.Statement = strconv.Itoa(i)
		queue.Actions = statement.GetAllActionsAsString()
		queue.Access = statement.GetAllPrincipalsAsString()
		queue.ConditionText = statement.GetConditionsInEnglish()

		if queue.ConditionText == "Default resource policy: Not exploitable\n" {
			//queue.Actions = ""
			//queue.Access = ""
			queue.ResourcePolicySummary = prefix + "Default resource policy: Not exploitable\n"
		} else if queue.ConditionText != "\n" && queue.ConditionText != "" {
			//queue.Actions = statement.GetAllActionsAsString()
			//queue.Access = statement.GetAllPrincipalsAsString()
			queue.ResourcePolicySummary = fmt.Sprintf("%s%s can %s when the following conditions are met: %s", prefix, strings.TrimSuffix(queue.Access, "\n"), queue.Actions, queue.ConditionText)

		} else {
			//queue.ResourcePolicySummary = queue.ConditionText
			//queue.ResourcePolicySummary = fmt.Sprintf("%s%s can %s when the following conditions are met: %s", prefix, strings.TrimSuffix(queue.Access, "\n"), queue.Actions, queue.ConditionText)
			queue.ResourcePolicySummary = fmt.Sprintf("%s%s can %s", prefix, strings.TrimSuffix(queue.Access, "\n"), queue.Actions)

		}

		dataReceiver <- *queue
	}
}

func (m *SQSModule) storeAccessPolicy(dir string, queue *Queue) {
	f := filepath.Join(m.getLootDir(), dir, fmt.Sprintf("%s.json", m.getQueueName(queue.URL)))

	if err := m.storeFile(f, queue.PolicyJSON); err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
	}
}

func (m *SQSModule) getLootDir() string {
	return filepath.Join(m.output.FilePath, "loot", "sqs-policies")
}

func (m *SQSModule) getQueueName(url string) string {
	tokens := strings.SplitN(url, "/", 5)
	if len(tokens) != 5 {
		return ""
	}
	return tokens[4]
}

func (m *SQSModule) storeFile(filename string, policy string) error {
	err := os.MkdirAll(filepath.Dir(filename), 0750)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("creating parent dirs: %s", err)
	}

	return os.WriteFile(filename, []byte(policy), 0644)

}
