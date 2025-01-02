package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/s3"
)

const (
	idCounterBase = 10000000
)

var (
	inputMap map[uint32]struct{}
)

type Message struct {
	Log string
}

func main() {
	// Define flags
	region := flag.String("region", "", "AWS Region")
	bucket := flag.String("bucket", "", "S3 Bucket Name")
	logGroup := flag.String("log-group", "", "CloudWatch Log Group Name")
	prefix := flag.String("prefix", "", "Log Prefix")
	destination := flag.String("destination", "", "Log Destination (s3 or cloudwatch)")
	inputRecord := flag.Int("input-record", 0, "Total input record number")
	logDelay := flag.String("log-delay", "", "Log delay")

	// Parse flags
	flag.Parse()

	// Validate required flags
	if *region == "" {
		exitErrorf("[TEST FAILURE] AWS Region required. Use the -region flag.")
	}
	if *bucket == "" {
		exitErrorf("[TEST FAILURE] Bucket name required. Use the -bucket flag.")
	}
	if *logGroup == "" {
		exitErrorf("[TEST FAILURE] Log group name required. Use the -log-group flag.")
	}
	if *prefix == "" {
		exitErrorf("[TEST FAILURE] Object prefix required. Use the -prefix flag.")
	}
	if *destination == "" {
		exitErrorf("[TEST FAILURE] Log destination for validation required. Use the -destination flag.")
	}
	if *inputRecord == 0 {
		exitErrorf("[TEST FAILURE] Total input record number required. Use the -input-record flag.")
	}
	if *logDelay == "" {
		exitErrorf("[TEST FAILURE] Log delay required. Use the -log-delay flag.")
	}

	// Map for counting unique records in corresponding destination
	inputMap = make(map[uint32]struct{}, *inputRecord)

	totalRecordFound := 0
	if *destination == "s3" {
		s3Client, err := getS3Client(*region)
		if err != nil {
			exitErrorf("[TEST FAILURE] Unable to create new S3 client: %v", err)
		}

		totalRecordFound = validate_s3(s3Client, *bucket, *prefix)
	} else if *destination == "cloudwatch" {
		cwClient, err := getCWClient(*region)
		if err != nil {
			exitErrorf("[TEST FAILURE] Unable to create new CloudWatch client: %v", err)
		}

		totalRecordFound = validate_cloudwatch(cwClient, *logGroup, *prefix)
	}

	// Get benchmark results based on log loss, log delay and log duplication
	get_results(*inputRecord, totalRecordFound, *logDelay)
}

// Creates a new S3 Client
func getS3Client(region string) (*s3.S3, error) {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region)},
	)

	if err != nil {
		return nil, err
	}

	return s3.New(sess), nil
}

// Validates the log messages. Our log producer is designed to write log records in a specific format.
// Log format generated by our producer: 8CharUniqueID_13CharTimestamp_RandomString (10029999_1639151827578_RandomString).
// Both of the Kinesis Streams and Kinesis Firehose try to send each log maintaining the "at least once" policy.
// To validate, we need to make sure all the log records from input file are stored at least once.
func validate_s3(s3Client *s3.S3, bucket string, prefix string) int {
	var continuationToken *string
	var input *s3.ListObjectsV2Input
	s3RecordCounter := 0
	s3ObjectCounter := 0

	for {
		input = &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: continuationToken,
			Prefix:            aws.String(prefix),
		}

		response, err := s3Client.ListObjectsV2(input)
		if err != nil {
			exitErrorf("[TEST FAILURE] Error occurred to get the objects from bucket: %q., %v", bucket, err)
		}

		for _, content := range response.Contents {
			input := &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    content.Key,
			}
			obj, err := s3Client.GetObject(input)
			if err != nil {
				exitErrorf("[TEST FAILURE] Error to get S3 object. %v", err)
			}
			s3ObjectCounter++

			// Directly unmarshal the JSON objects from the S3 object body
			decoder := json.NewDecoder(obj.Body)
			for {
				var message Message
				err := decoder.Decode(&message)
				if err == io.EOF {
					break
				}
				if err != nil {
					fmt.Println("[TEST ERROR] Malform log entry. Unmarshal Error:", err)
					continue
				}

				recordId := message.Log[:8]
				s3RecordCounter++
				value, err := strconv.ParseUint(recordId, 10, 32)
				if err != nil {
					fmt.Println("[TEST ERROR] Malform log entry. ParseUint Error:", err)
					continue
				}
				recordIdUint := uint32(value)
				inputMap[recordIdUint] = struct{}{}
			}

			// Close the S3 object body
			obj.Body.Close()
		}

		if !aws.BoolValue(response.IsTruncated) {
			break
		}
		continuationToken = response.NextContinuationToken
	}

	fmt.Println("total_s3_obj, ", s3ObjectCounter)

	return s3RecordCounter
}

func processFile(file *os.File, filePath string) (int, error) {
	var err error
	file, err = os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()
	var message Message
	var recordId string

	localCounter := 0
	// Directly unmarshal the JSON objects from the S3 object body
	decoder := json.NewDecoder(file)
	for {
		err = decoder.Decode(&message)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Println("[TEST ERROR] Malform log entry. Unmarshal Error:", err)
			continue
		}

		recordId = message.Log[:8]
		value, err := strconv.ParseUint(recordId, 10, 32)
		if err != nil {
			fmt.Println("[TEST ERROR] Malform log entry. ParseUint Error:", err)
			continue
		}
		recordIdUint := uint32(value)
		inputMap[recordIdUint] = struct{}{}
	}

	return localCounter, nil
}

// Creates a new CloudWatch Client
func getCWClient(region string) (*cloudwatchlogs.CloudWatchLogs, error) {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region)},
	)

	if err != nil {
		return nil, err
	}

	return cloudwatchlogs.New(sess), nil
}

// Validate logs in CloudWatch.
// Similar logic as S3 validation.
func validate_cloudwatch(cwClient *cloudwatchlogs.CloudWatchLogs, logGroup string, logStream string) int {
	var forwardToken *string
	var input *cloudwatchlogs.GetLogEventsInput
	cwRecoredCounter := 0

	// Returns all log events from a CloudWatch log group with the given log stream.
	// This approach utilizes NextForwardToken to pull all log events from the CloudWatch log group.
	for {
		if forwardToken == nil {
			input = &cloudwatchlogs.GetLogEventsInput{
				LogGroupName:  aws.String(logGroup),
				LogStreamName: aws.String(logStream),
				StartFromHead: aws.Bool(true),
				Limit:         aws.Int64(10000),
			}
		} else {
			input = &cloudwatchlogs.GetLogEventsInput{
				LogGroupName:  aws.String(logGroup),
				LogStreamName: aws.String(logStream),
				NextToken:     forwardToken,
				StartFromHead: aws.Bool(true),
				Limit:         aws.Int64(10000),
			}
			// Sleep between GetLogEvents calls to avoid throttling
			time.Sleep(100 * time.Millisecond)
		}

		response, err := cwClient.GetLogEvents(input)
		for err != nil {
			// retry for throttling exception
			if strings.Contains(err.Error(), "ThrottlingException: Rate exceeded") {
				time.Sleep(5 * time.Second)
				response, err = cwClient.GetLogEvents(input)
			} else {
				exitErrorf("[TEST FAILURE] Error occured to get the log events from log group: %q., %v", logGroup, err)
			}
		}

		for _, event := range response.Events {
			log := aws.StringValue(event.Message)

			// First 8 char is the unique record ID
			recordId := log[:8]
			value, err := strconv.ParseUint(recordId, 10, 32)
			if err != nil {
				fmt.Println("Error:", err)
				continue
			}
			recordIdUint := uint32(value)
			cwRecoredCounter += 1
			inputMap[recordIdUint] = struct{}{}
		}

		// Same NextForwardToken will be returned if we reach the end of the log stream
		if aws.StringValue(response.NextForwardToken) == aws.StringValue(forwardToken) {
			break
		}

		forwardToken = response.NextForwardToken
	}

	return cwRecoredCounter
}

func get_results(totalInputRecord int, totalRecordFound int, logDelay string) {
	uniqueRecordFound := len(inputMap)

	fmt.Println("total_input, ", totalInputRecord)
	fmt.Println("total_destination, ", totalRecordFound)
	fmt.Println("unique, ", uniqueRecordFound)
	fmt.Println("duplicate, ", (totalRecordFound - uniqueRecordFound))
	fmt.Println("delay, ", logDelay)
	fmt.Println("percent_loss, ", (totalInputRecord-uniqueRecordFound)*100/totalInputRecord) // %

	if totalInputRecord != uniqueRecordFound {
		fmt.Println("missing, ", totalInputRecord-uniqueRecordFound)
	} else {
		fmt.Println("missing, ", 0)
	}
}

func exitErrorf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}
