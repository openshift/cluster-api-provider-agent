/*
Copyright 2021.

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

package controllers

import (
	"context"

	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	BackupLabel      = "cluster.open-cluster-management.io/backup"
	BackupLabelValue = "true"
)

func ensureSecretLabel(ctx context.Context, c client.Client, secret *corev1.Secret) error {
	if secret == nil {
		return nil
	}
	// Add backup label to the secret if not present
	if !metav1.HasLabel(secret.ObjectMeta, BackupLabel) {
		metav1.SetMetaDataLabel(&secret.ObjectMeta, BackupLabel, BackupLabelValue)
		err := c.Update(ctx, secret)
		if err != nil {
			return errors.Wrapf(err, "failed to set label %s:%s for secret %s/%s", BackupLabel, BackupLabelValue, secret.Namespace, secret.Name)
		}
	}
	return nil
}

func ensureBackupLabel(obj *metav1.ObjectMeta) {
	if obj == nil {
		return
	}
	if !metav1.HasLabel(*obj, BackupLabel) {
		metav1.SetMetaDataLabel(obj, BackupLabel, BackupLabelValue)
	}
}

func GetKubeClientSchemes(schemes *runtime.Scheme) *runtime.Scheme {
	utilruntime.Must(scheme.AddToScheme(schemes))
	utilruntime.Must(corev1.AddToScheme(schemes))
	utilruntime.Must(aiv1beta1.AddToScheme(schemes))
	utilruntime.Must(hivev1.AddToScheme(schemes))
	utilruntime.Must(hiveext.AddToScheme(schemes))
	utilruntime.Must(clusterv1.AddToScheme(schemes))
	return schemes
}
