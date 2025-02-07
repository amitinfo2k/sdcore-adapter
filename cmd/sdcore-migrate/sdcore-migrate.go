// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package main

/*
 * Main file for data migration
 *
 * Example invocation:
 *
 * /usr/local/bin/sdcore-migrate --from-target connectivity-service-v1 --to-target connectivity-service-v2 --from-version 1.0.0 --to-version 2.0.0 --aether-config onos-config:5150 -client_key=/etc/sdcore-adapter/certs/tls.key -client_crt=/etc/sdcore-adapter/certs/tls.crt -ca_crt=/etc/sdcore-adapter/certs/tls.cacert -hostCheckDisabled
 */

import (
	"flag"
	modelsv2 "github.com/onosproject/aether-models/models/aether-2.0.x/api"
	modelsv4 "github.com/onosproject/aether-models/models/aether-4.x/api"
	"github.com/onosproject/sdcore-adapter/internal/pkg/version"
	"github.com/onosproject/sdcore-adapter/pkg/gnmi"
	"github.com/onosproject/sdcore-adapter/pkg/gnmiclient"
	"github.com/onosproject/sdcore-adapter/pkg/migration/steps"
	"github.com/openconfig/ygot/ygot"
	"reflect"
	"strings"
	"time"

	"github.com/onosproject/onos-lib-go/pkg/logging"
	"github.com/onosproject/sdcore-adapter/pkg/migration"
)

var (
	fromTarget       = flag.String("from-target", "", "target device to migrate from")
	toTarget         = flag.String("to-target", "", "target device to migrate to")
	fromVersion      = flag.String("from-version", "", "modeling version to migrate from")
	toVersion        = flag.String("to-version", "", "modeling version to migrate to")
	aetherConfigAddr = flag.String("aether-config", "", "address of aether-config e.g. onos-config:5150")
	outputToGnmi     = flag.Bool("out-to-gnmi", false, "output to aetherConfig as gnmi calls")
	output           = flag.String("o", "", "filename to send output to instead of STDOUT when out-to-gnmi not set")
)

var log = logging.GetLogger("sdcore-migrate")

func main() {
	flag.Parse()

	log.Infof("sdcore-migrate")
	version.LogVersion("  ")

	gnmiClient, err := gnmiclient.NewGnmi(*aetherConfigAddr, time.Second*5)
	if err != nil {
		log.Fatalf("Error opening gNMI client %s", err.Error())
	}
	defer gnmiClient.CloseClient()

	v4Models := gnmi.NewModel(modelsv4.ModelData(),
		reflect.TypeOf((*modelsv4.Device)(nil)),
		modelsv4.SchemaTree["Device"],
		modelsv4.Unmarshal,
		//models.ΛEnum  // NOTE: There is no Enum in the aether models? So use a blank map.
		map[string]map[int64]ygot.EnumDefinition{},
	)

	v2Models := gnmi.NewModel(modelsv2.ModelData(),
		reflect.TypeOf((*modelsv2.Device)(nil)),
		modelsv2.SchemaTree["Device"],
		modelsv2.Unmarshal,
		//models.ΛEnum  // NOTE: There is no Enum in the aether models? So use a blank map.
		map[string]map[int64]ygot.EnumDefinition{},
	)

	// Initialize the migration engine and register migration steps.
	mig := migration.NewMigrator(gnmiClient)
	mig.AddMigrationStep("4.0.0", v4Models, "2.0.0", v2Models, steps.MigrateV4V2)

	if *fromVersion == "" {
		log.Fatalf("--from-version not specified. Supports: %s", strings.Join(mig.SupportedVersions(), ", "))
	} else if *toVersion == "" {
		log.Fatalf("--to-version not specified. Supports: %s", strings.Join(mig.SupportedVersions(), ", "))
	} else if *aetherConfigAddr == "" {
		log.Fatal("--aether-config not specified")
	} else if *fromTarget == "" {
		log.Fatal("--from-target not specified")
	} else if *toTarget == "" {
		log.Fatal("--to-target not specified")
	}

	// Perform the migration
	if err = mig.Migrate(*fromTarget, *fromVersion, *toTarget, *toVersion, outputToGnmi, output); err != nil {
		log.Fatal("Migration failed. %s", err.Error())
	}
}
