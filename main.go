// Copyright (c) 2016, M Bogus.
// This source file is part of the KUBE-AMQP-AUTOSCALE open source project
// Licensed under Apache License v2.0
// See LICENSE file for license information

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
)

func init() {
	setVersion()
	flag.StringVar(&brokerURIParam, "amqp-uri", "", "RabbitMQ broker URI")
	flag.StringVar(&queueNameParam, "amqp-queue", "", "RabbitMQ queue to measure load on an application")
	flag.StringVar(&apiURLParam, "api-url", "", "Kubernetes API URL")
	flag.StringVar(&apiUserParam, "api-user", "", "username for basic authentication on Kubernetes API")
	flag.StringVar(&apiPasswdParam, "api-passwd", "", "password for basic authentication on Kubernetes API")
	flag.StringVar(&apiTokenParam, "api-token", "", "path to a bearer token file for OAuth authentication")
	flag.StringVar(&apiCAFileParam, "api-cafile", "", "path to CA certificate file for HTTPS connections")
	flag.BoolVar(&apiInsecureParam, "api-insecure", false, "set to `true` for connecting to Kubernetes API without verifying TLS certificate; unsafe, use for development only")
	flag.IntVar(&minParam, "min", 1, "lower limit for the number of replicas for a Kubernetes pod that can be set by the autoscaler")
	flag.IntVar(&maxParam, "max", -1, "upper limit for the number of replicate for a Kubernetes pod that can be set by the autoscaler")
	flag.StringVar(&nameParam, "name", "", "name of the Kubernetes resource to autoscale")
	flag.StringVar(&kindParam, "kind", "Deployment", "type of the Kubernetes resource to autoscale")
	flag.StringVar(&namespaceParam, "ns", "default", "Kubernetes namespace")
	flag.IntVar(&intervalParam, "interval", 30, "time interval between Kubernetes resource scale runs in secs")
	flag.IntVar(&thresholdParam, "threshold", -1, "number of messages on a queue representing maximum load on the autocaled Kubernetes resource")
	flag.IntVar(&increaseLimitParam, "increase-limit", -1, "number of messages on a queue representing maximum load on the autocaled Kubernetes resource")
	flag.IntVar(&decreaseLimitParam, "decrease-limit", -1, "number of messages on a queue representing maximum load on the autocaled Kubernetes resource")
	flag.IntVar(&statsIntervalParam, "stats-interval", 5, "time interval between metrics gathering runs in seconds")
	flag.IntVar(&evalIntervalsParam, "eval-intervals", 2, "number of autoscale intervals used to calculate average queue length")
	flag.Float64Var(&statsCoverageParam, "stats-coverage", 0.75, "required percentage of statistics to calculate average queue length")
	flag.StringVar(&dbFileParam, "db", ":memory:", "sqlite3 database filename")
	flag.StringVar(&dbDirParam, "db-dir", "", "directory for sqlite3 statistics database file")

	flag.BoolVar(&version, "version", false, "show version")
}

var (
	version            bool
	brokerURIParam     string
	queueNameParam     string
	apiURLParam        string
	apiUserParam       string
	apiPasswdParam     string
	apiTokenParam      string
	apiCAFileParam     string
	apiInsecureParam   bool
	minParam           int
	maxParam           int
	nameParam          string
	kindParam          string
	namespaceParam     string
	intervalParam      int
	thresholdParam     int
	increaseLimitParam int
	decreaseLimitParam int
	evalIntervalsParam int
	statsCoverageParam float64
	statsIntervalParam int
	dbFileParam        string
	dbDirParam         string
)

func validateParams() error {
	if len(brokerURIParam) == 0 {
		return errors.New("Missing RabbitMQ URI")
	}
	if len(queueNameParam) == 0 {
		return errors.New("Missing RabbitMQ queue name")
	}
	if len(apiURLParam) == 0 {
		return errors.New("Missing Kubernetes API URL")
	}
	if intervalParam < 1 {
		return fmt.Errorf("Invalid auto-scale interval '%d'", intervalParam)
	}
	if thresholdParam < 1 {
		return fmt.Errorf("Invalid threshold value '%d'", thresholdParam)
	}
	if intervalParam <= statsIntervalParam {
		return fmt.Errorf("Interval for saving statistics '%d' should be smaller than auto-scale interval '%d'", statsIntervalParam, intervalParam)
	}
	if statsCoverageParam > 1.0 || statsCoverageParam < 0.0 {
		return fmt.Errorf("Invalid metrics coverage ratio '%.2f'", statsCoverageParam)
	}
	if minParam < 0 {
		return fmt.Errorf("Invalid lower limit for the number of pods '%d'", minParam)
	}
	if maxParam <= minParam {
		return fmt.Errorf("Upper limit for the number of pods '%d' must be greater than lower limit '%d'", maxParam, minParam)
	}
	if len(nameParam) == 0 {
		return errors.New("Missing name of the resource to autoscale")
	}
	if len(kindParam) == 0 {
		return errors.New("Missing kind of the resource to autoscale")
	}
	switch kindParam {
	case replicationControllerKind, replicaSetKind, deploymentKind:
	default:
		return fmt.Errorf("Invalid kind of the resource '%s'", kindParam)
	}
	if len(namespaceParam) == 0 {
		return errors.New("Missing namespace of the resource to autoscale")
	}

	return nil
}

const appName = "Kubernetes AMQP Autoscaler"

func main() {
	flag.Parse()

	if version {
		fmt.Printf("%s %s\n", appName, appVersion)
		fmt.Printf("go version: %s\n", runtime.Version())
		os.Exit(0)
	}

	if err := validateParams(); err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting %s %s", appName, appVersion)
	log.Printf("System with %d CPUs and environment with %d max processes",
		runtime.NumCPU(), runtime.GOMAXPROCS(0))

	db, err := connectToDB(&dbFileParam)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	createTable(db)

	forever := make(chan struct{}, 1)

	duration := evalIntervalsParam * intervalParam
	fsample := func(n int) error { return updateMetrics(db, n, duration) }
	go monitorQueue(brokerURIParam, queueNameParam, statsIntervalParam, fsample, forever)

	fmetrics := func() (*queueMetrics, error) {
		return getMetrics(db, duration, statsIntervalParam)
	}

	fscale := func(newSize int32) error {
		bounds := &scaleBounds{Min: minParam,
			Max:           maxParam,
			IncreaseLimit: increaseLimitParam,
			DecreaseLimit: decreaseLimitParam}
		return scale(kindParam, namespaceParam, nameParam, newSize,
			&apiContext{URL: apiURLParam,
				User:      apiUserParam,
				Passwd:    apiPasswdParam,
				TokenFile: apiTokenParam,
				CAFile:    apiCAFileParam,
				Insecure:  apiInsecureParam,
				Bounds:    bounds,
			})
	}

	go autoscale(fmetrics,
		&scaleContext{Threshold: thresholdParam,
			Coverage: statsCoverageParam,
			Interval: intervalParam,
			Scaler:   fscale},
		forever)

	<-forever
}

// setVersion figures out the version information based on
// variables set by -ldflags.
func setVersion() {
	// release build
	if Version != "" && Build != "" {
		if BuildType == "RELEASE" {
			appVersion = fmt.Sprintf("%s (%s)", Version, Build)
		} else if BuildType == "SNAPSHOT" {
			appVersion = fmt.Sprintf("%s-%s (%s)", Version,
				BuildType, Build)
		} else {
			appVersion = fmt.Sprintf("%s-%s (+%s %s)", Version,
				BuildType, Build, BuildDate)
		}
	}
}

// Build information obtained with the help of -ldflags
var (
	appVersion = "(untracked build)" // inferred at startup

	Version   string // release line version e.g. 1.0
	Build     string // git rev-parse HEAD
	BuildType string // RELEASE, SNAPSHOT, DEV
	BuildDate string // DEV builds only: date +%FT%T%z
)