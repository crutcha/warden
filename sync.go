package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	log "github.com/sirupsen/logrus"
)

var (
	// TODO: is there some better way to allow for stubbing filesystem interactions for tests?
	concreteWalkFunc = walkDirectory
)

type ObjectRequests struct {
	TombstoneKeys []string
	UploadKeys    map[string]string
}

/*
type UploadResult struct {
	Upload    sync.Map
	Tombstone sync.Map
}
*/

type ResultMap struct {
	Upload    map[string]error
	Tombstone map[string]error
	lock      *sync.Mutex
}

func (r *ResultMap) AddUploadResult(key string, result error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.Upload[key] = result
}

func (r *ResultMap) AddTombstoneResult(key string, result error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.Tombstone[key] = result
}

func doSync(client BucketClient, sc SyncConfig, lock *sync.Mutex) (*ResultMap, error) {
	resultMap := &ResultMap{
		Upload:    make(map[string]error),
		Tombstone: make(map[string]error),
		lock:      new(sync.Mutex),
	}
	if !lock.TryLock() {
		log.Warn("Another sync routine is already running. Skipping.")
		return resultMap, fmt.Errorf("Unable to acquire sync lock")
	}
	defer lock.Unlock()
	syncStartTime := time.Now()

	// TODO: for now with a small number of exclusion matchers, this OK, but we should figure out
	// a more efficient way to do this to handle a larger amount of exception patterns
	regexStr := strings.Join(sc.Exclude, "|")
	exclude := regexp.MustCompile(regexStr)

	objectRequests := ObjectRequests{
		TombstoneKeys: make([]string, 0),
		UploadKeys:    make(map[string]string),
	}

	// TODO: probably some better way to handle this
	// since gocron is setup with a pointer to a synchandler, we need to flush
	// the state from the last time it ran
	bucketFiles, listBucketErr := client.ListObjects(sc.DestinationBucket)
	if listBucketErr != nil {
		return resultMap, fmt.Errorf("Error listing S3 bucket: %s", listBucketErr)
	}
	localFiles, listLocalFilesErr := concreteWalkFunc(sc.SourceFolder)
	if listLocalFilesErr != nil {
		return resultMap, fmt.Errorf("Error walking local directory: %s", listLocalFilesErr)
	}

	for localPath, localFileInfo := range localFiles {
		isExcluded := len(sc.Exclude) != 0 && exclude.MatchString(localPath)
		if isExcluded {
			log.Info(fmt.Sprintf("%s matches exclusion list. skipping...", localPath))
			continue
		}

		pathComponents := strings.Split(localPath, sc.SourceFolder)
		// TODO: ensure we have at least 2 components?
		uploadKey := pathComponents[1]
		remoteObj, ok := bucketFiles[strings.TrimPrefix(uploadKey, "/")]

		// S3 will apply it's own last modified timestamp when an object is uploaded, the timestamp from local file
		// stat wont match. As long as the last modified timestamp from S3 for any given file/key combo is more recent
		// than the local file last modified timestamp, S3 has the most recent copy. we could use our own metadata
		// to track local file modification time, but this would require a HeadObject call for every file, and on
		// a large drive/bucket, that's a ton of API calls which both slow this down considerably and cost more.
		if !ok {
			objectRequests.UploadKeys[uploadKey] = localPath
		} else {
			localFileSize := localFileInfo.Size()
			timeSinceUpdate := remoteObj.LastModified.Sub(localFileInfo.ModTime())
			if timeSinceUpdate < 0 || localFileSize != remoteObj.Size {
				log.Info(fmt.Sprintf("%s has been modified, will update", localPath))
				objectRequests.UploadKeys[uploadKey] = localPath
			} else {
				log.Debug(fmt.Sprintf("%s is in sync, no action required", localPath))
			}
		}
	}

	for key, _ := range bucketFiles {
		if !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		localPathPrefix := strings.TrimSuffix(sc.SourceFolder, "/")
		localPathForKey := localPathPrefix + key
		_, ok := localFiles[localPathForKey]
		if !ok {
			objectRequests.TombstoneKeys = append(objectRequests.TombstoneKeys, key)
			log.Info(fmt.Sprintf("%s exists in bucket but not locally, will tombstone", key))
		}
	}

	syncObjectRequests(client, objectRequests, resultMap, sc.DestinationBucket, sc.TombstoneBucket)
	syncEndTime := time.Now()
	duration := syncEndTime.Sub(syncStartTime)
	log.Info(fmt.Sprintf("Sync complete for %s. Took %s", sc.SourceFolder, duration.String()))

	/*
		if s.snsTopic != "" {
			notifyErr := notifySyncResultsViaSns(s.snsClient, s.snsTopic, objectRequests)
			if notifyErr != nil {
				log.Warn(fmt.Sprintf("Error notifying sync results: %s", notifyErr))
			}
		}
	*/
	notifySyncResultsViaSns(resultMap)

	return resultMap, nil
}

func syncObjectRequests(client BucketClient, objReqs ObjectRequests, resultMap *ResultMap, destBucket, tombstoneBucket string) {
	// TODO: from app config
	semaphore := make(chan int, 5)
	var wg sync.WaitGroup

	for fileKey, fileInfo := range objReqs.UploadKeys {
		wg.Add(1)
		go doUploadFile(client, destBucket, fileKey, fileInfo, semaphore, &wg, resultMap)
	}

	if tombstoneBucket != "" {
		for _, key := range objReqs.TombstoneKeys {
			wg.Add(1)
			go doTombstoneObject(client, destBucket, tombstoneBucket, key, semaphore, &wg, resultMap)
		}
	}

	wg.Wait()
	//return SyncResults{}
}

func doUploadFile(
	client BucketClient,
	bucket, key, filePath string,
	semaphore chan int,
	wg *sync.WaitGroup,
	resultMap *ResultMap,
) error {
	resultMap.AddUploadResult(key, nil)
	semaphore <- 1
	defer wg.Done()

	fd, fileErr := os.Open(filePath)
	if fileErr != nil {
		resultMap.AddUploadResult(key, fileErr)
		<-semaphore
		return fileErr
	}
	defer fd.Close()

	log.Info(fmt.Sprintf("Uploading file %s as key %s", filePath, key))
	key = strings.TrimPrefix(key, "/")
	uploadErr := client.UploadFile(bucket, key, fd)
	<-semaphore

	return uploadErr
}

func doTombstoneObject(
	client BucketClient,
	sourceBucket, destinationBucket, key string,
	semaphore chan int,
	wg *sync.WaitGroup,
	resultMap *ResultMap,
) error {
	resultMap.AddTombstoneResult(key, nil)
	semaphore <- 1
	defer wg.Done()

	copyErr := client.CopyObject(sourceBucket, destinationBucket, key)

	if copyErr != nil {
		log.Warn(fmt.Sprintf("Error copying object during tombstone routine: %s", copyErr))
		resultMap.AddTombstoneResult(key, copyErr)
		<-semaphore
		return copyErr
	}

	delErr := client.DeleteObject(destinationBucket, key)

	if delErr != nil {
		log.Warn(fmt.Sprintf("Error deleting original object during tombstone routine: %s", delErr))
		resultMap.AddTombstoneResult(key, delErr)
		<-semaphore
		return delErr
	}

	<-semaphore
	return nil
}

func doBackup(client BucketClient, bc BackupConfig) {
	// TODO: move file walk into util that returns list of paths
	filesToCompress := make([]string, 0)
	walkErr := filepath.Walk(bc.SourceFolder, func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			filesToCompress = append(filesToCompress, path)
		}
		return nil
	})
	if walkErr != nil {
		log.Error(fmt.Sprintf("Backup directory walk failed: %s", walkErr))

	}

	now := time.Now()
	backupTimestamp := now.Format(time.RFC3339)
	keyBase := strings.ReplaceAll(bc.SourceFolder, "/", "_")
	backupPrefix := fmt.Sprintf("%s_%s_*.tar.gz", strings.TrimPrefix(keyBase, "_"), backupTimestamp)
	tarFile, _ := ioutil.TempFile(os.TempDir(), backupPrefix)
	defer os.Remove(tarFile.Name())

	log.Info(fmt.Sprintf("Creating backup tarball: %s", tarFile.Name()))
	createArchive(filesToCompress, tarFile)

	// this is janky but the way this is written, this file descripter would be closed already
	// so....we need to open the file again
	uploadFile, uploadFileOpenErr := os.Open(tarFile.Name())
	if uploadFileOpenErr != nil {
		log.Warn("Error uploading backup: ", uploadFileOpenErr)
		return
	}
	defer uploadFile.Close()

	fileKey := filepath.Base(tarFile.Name())
	putErr := client.UploadFile(bc.DestinationBucket, fileKey, uploadFile)
	if putErr != nil {
		log.Warn("Backup upload error: ", putErr)
	} else {
		log.Info("Upload succeded for ", fileKey)
	}
}

// TODO: this doesn't actually capture if upload or tombstone operations were successful or
// returned an error. need to update this func to accomodate for each individual key result.
func notifySyncResultsViaSns(resultMap *ResultMap) error {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithSharedConfigProfile("nass3sync"),
		config.WithRegion("us-east-2"))

	fmt.Println(err)
	snsClient := sns.NewFromConfig(cfg)

	requiresNotification := len(resultMap.Tombstone) != 0 || len(resultMap.Upload) != 0
	// we only want to notify if something actually happened
	if !requiresNotification {
		return nil
	}

	notificationBody := "Uploads:\n"
	for key, uploadErr := range resultMap.Upload {
		notificationBody += fmt.Sprintf("  - %s => %v\n", key, uploadErr)
	}

	notificationBody += "\n\nTombstones:\n"
	for key, tombstoneErr := range resultMap.Tombstone {
		notificationBody += fmt.Sprintf("  - %s => %v\n", key, tombstoneErr)
	}

	snsPublishReq := &sns.PublishInput{
		Message:  aws.String(notificationBody),
		TopicArn: aws.String("arn:aws:sns:us-east-2:719670394721:nass3sync"),
	}
	_, publishErr := snsClient.Publish(context.TODO(), snsPublishReq)

	return publishErr
}
