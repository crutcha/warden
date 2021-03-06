package main

import (
	"flag"
	"fmt"
	"sync"

	//"github.com/davecgh/go-spew/spew"
	"time"

	"github.com/go-co-op/gocron"
	log "github.com/sirupsen/logrus"
)

func main() {
	configFilePath := flag.String("configfile", "/etc/warden.yml", "Configuration File Path")
	debugLogging := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	logFormatter := new(log.TextFormatter)
	logFormatter.TimestampFormat = "2006-01-02 15:04:05"
	logFormatter.FullTimestamp = true
	log.SetFormatter(logFormatter)
	if *debugLogging {
		log.SetLevel(log.DebugLevel)
	}

	appConfig, configErr := InitAppConfig(*configFilePath)
	if configErr != nil {
		log.Fatal(configErr)
	}

	log.Info("----------")
	log.Info("Starting with Config: ")
	for _, element := range appConfig.ConfigStringArray() {
		log.Info(element)
	}
	log.Info("----------")

	bucketClient, clientErr := BucketClientFromConfig(appConfig)
	notifier, notifierErr := NotifierFromConfig(appConfig)
	if clientErr != nil {
		log.Fatalf("Error creating bucket client from config: %s", clientErr)
	}
	if notifierErr != nil {
		log.Fatalf("Error creating notifier: %s", notifierErr)
	}

	scheduler := gocron.NewScheduler(time.UTC)

	for _, sc := range appConfig.Sync {
		syncLock := &sync.Mutex{}
		//var syncLock sync.Mutex
		scJob, scErr := scheduler.Every(sc.Interval).Minutes().Do(
			doSync,
			bucketClient,
			sc,
			notifier,
			syncLock,
		)
		if scErr != nil {
			log.Fatal(fmt.Errorf("Error setting up sync job for %s: %s", sc.SourceFolder, scErr))
		}
		logString := fmt.Sprintf(
			"Scheduled sync for folder %s. Next run at: %s",
			sc.SourceFolder,
			scJob.ScheduledTime().String(),
		)
		log.Info(logString)
	}

	for _, bc := range appConfig.Backup {
		bcJob, bcErr := scheduler.Cron(bc.At).Do(doBackup, bucketClient, bc, notifier)
		if bcErr != nil {
			log.Fatal(bcErr)
		}
		logString := fmt.Sprintf(
			"Scheduled backup for folder %s. Next run at: %s",
			bc.SourceFolder,
			bcJob.ScheduledTime().String(),
		)
		log.Info(logString)
	}

	scheduler.StartBlocking()
}
