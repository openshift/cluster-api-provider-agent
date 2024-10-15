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

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:generate mockgen -source=remote_client.go -package=controllers -destination=mock_remote_client.go
type RemoteClientHandler interface {
	GetRemoteClient(ctx context.Context, secretNamespace string) (client.Client, error)
}

type remoteClient struct {
	localClient client.Client
	scheme      *runtime.Scheme
}

func NewRemoteClient(localClient client.Client, scheme *runtime.Scheme) RemoteClientHandler {
	scheme = GetKubeClientSchemes(scheme)
	return &remoteClient{localClient: localClient, scheme: scheme}
}

func (r *remoteClient) GetRemoteClient(ctx context.Context, secretNamespace string) (client.Client, error) {
	secretKey := client.ObjectKey{
		Namespace: secretNamespace,
		Name:      "admin-kubeconfig",
	}
	secret := corev1.Secret{}
	if err := r.localClient.Get(ctx, secretKey, &secret); err != nil {
		return nil, errors.Wrapf(err, "failed to get kubeconfig secret")
	}
	if secret.Data == nil {
		return nil, errors.Errorf("Secret %s/%s  does not contain any data", secret.Namespace, secret.Name)
	}
	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok || len(kubeconfigData) == 0 {
		return nil, errors.Errorf("Secret data for %s/%s  does not contain kubeconfig", secretKey.Namespace, secretKey.Name)
	}

	// error labeling secret shouldn't prevent this function from succeeding
	ensureSecretLabel(ctx, r.localClient, &secret) //nolint:errcheck

	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfigData)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get clientconfig from kubeconfig data in secret")
	}
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get restconfig for remote kube client")
	}

	targetClient, err := client.New(restConfig, client.Options{Scheme: r.scheme})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get remote kube client")
	}
	return targetClient, nil
}
