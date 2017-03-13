#!/bin/bash
#
# Deletes the compute instance for skia-android-master-perf.
#
set -x

source vm_config.sh

gcloud compute instances delete \
  --project=$PROJECT_ID \
  --delete-disks "boot" \
  --zone=$ZONE \
  $INSTANCE_NAME
