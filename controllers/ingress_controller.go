/*
Copyright 2022.

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
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// One of the Tunne CRD, ID, Name is mandatory
	// Tunnel CRD Name
	tunnelCRDAnnotation = "tunnels.networking.cfargotunnel.com"
	// Tunnel ID matching Tunnel Resource
	tunnelIdAnnotation = "tunnels.networking.cfargotunnel.com/id"
	// Tunnel Name matching Tunnel Resource Spec
	tunnelNameAnnotation = "tunnels.networking.cfargotunnel.com/name"
	// FQDN to create a DNS entry for and route traffic from internet on, defaults to Ingress host subdomain + cloudflare domain
	fqdnAnnotation = "tunnels.networking.cfargotunnel.com/fqdn"
	// If this annotation is set to false, do not limit searching Tunnel to Ingress namespace, and pick the 1st one found (Might be random?)
	// If set to anything other than false, use it as a namspace where Tunnel exists
	tunnelNSAnnotation = "tunnels.networking.cfargotunnel.com/ns"

	tunnelFinalizerAnnotation = "tunnels.networking.cfargotunnel.com/finalizer"
	tunnelDomainAnnotation    = "tunnels.networking.cfargotunnel.com/domain"
	configmapKey              = "config.yaml"
)

// IngressReconciler reconciles a Ingress object
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;update;patch

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Fetch Ingress from API
	ingress := &networkingv1.Ingress{}

	if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
		if apierrors.IsNotFound(err) {
			// Ingress object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Ingress deleted, nothing to do")
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Ingress")
		return ctrl.Result{}, err
	}

	// Read Ingress annotations. If both annotations are not set, return without doing anything
	tunnelName, okName := ingress.Annotations[tunnelNameAnnotation]
	tunnelId, okId := ingress.Annotations[tunnelIdAnnotation]
	fqdn := ingress.Annotations[fqdnAnnotation]
	tunnelNS, okNS := ingress.Annotations[tunnelNSAnnotation]
	tunnelCRD, okCRD := ingress.Annotations[tunnelCRDAnnotation]

	if !(okCRD || okName || okId) {
		// If an ingress with annotation is edited to remove just annotations, cleanup wont happen.
		// Not an issue as such, since it will be overwritten the next time it is used.
		log.Info("No related annotations not found, skipping Ingress")
		// Check if our finalizer is present on a non managed resource and remove it. This can happen if annotations were removed from the Ingress.
		if controllerutil.ContainsFinalizer(ingress, tunnelFinalizerAnnotation) {
			log.Info("Finalizer found on unmanaged Ingress, removing it")
			controllerutil.RemoveFinalizer(ingress, tunnelFinalizerAnnotation)
			err := r.Update(ctx, ingress)
			if err != nil {
				log.Error(err, "unable to remove finalizer from unmanaged Ingress")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// listOpts to search for ConfigMap. Set labels, and namespace restriction if
	listOpts := []client.ListOption{}
	labels := map[string]string{}
	if okId {
		labels[tunnelIdAnnotation] = tunnelId
	}
	if okName {
		labels[tunnelNameAnnotation] = tunnelName
	}
	if okCRD {
		labels[tunnelCRDAnnotation] = tunnelCRD
	}

	if tunnelNS == "true" || !okNS {
		labels[tunnelNSAnnotation] = ingress.Namespace
		listOpts = append(listOpts, client.InNamespace(ingress.Namespace))
	} else if okNS && tunnelNS != "false" {
		labels[tunnelNSAnnotation] = tunnelNS
		listOpts = append(listOpts, client.InNamespace(tunnelNS))
	} // else, no filter on namespace, pick the 1st one

	listOpts = append(listOpts, client.MatchingLabels(labels))

	log.Info("setting tunnel", "listOpts", listOpts)

	// Check if Ingress is marked for deletion
	if ingress.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(ingress, tunnelFinalizerAnnotation) {
			// Run finalization logic. If the finalization logic fails,
			// don't remove the finalizer so that we can retry during the next reconciliation.

			if err := r.configureCloudflare(log, ctx, ingress, fqdn, listOpts, true); err != nil {
				return ctrl.Result{}, err
			}

			// Remove tunnelFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(ingress, tunnelFinalizerAnnotation)
			err := r.Update(ctx, ingress)
			if err != nil {
				log.Error(err, "unable to continue with Ingress deletion")
				return ctrl.Result{}, err
			}
		}
	} else {
		// Add finalizer for Ingress
		if !controllerutil.ContainsFinalizer(ingress, tunnelFinalizerAnnotation) {
			controllerutil.AddFinalizer(ingress, tunnelFinalizerAnnotation)
			if err := r.Update(ctx, ingress); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Configure ConfigMap
		if err := r.configureCloudflare(log, ctx, ingress, fqdn, listOpts, false); err != nil {
			log.Error(err, "unable to configure ConfigMap", "key", configmapKey)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) getConfigMapConfiguration(ctx context.Context, log logr.Logger, listOpts []client.ListOption) (corev1.ConfigMap, Configuration, error) {
	// Fetch ConfigMap from API
	configMapList := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMapList, listOpts...); err != nil {
		log.Error(err, "Failed to list ConfigMaps", "listOpts", listOpts)
		return corev1.ConfigMap{}, Configuration{}, err
	}
	if len(configMapList.Items) == 0 {
		err := fmt.Errorf("no configmaps found")
		log.Error(err, "Failed to list ConfigMaps", "listOpts", listOpts)
		return corev1.ConfigMap{}, Configuration{}, err
	}
	configmap := configMapList.Items[0]

	// Read ConfigMap YAML
	configStr, ok := configmap.Data[configmapKey]
	if !ok {
		err := fmt.Errorf("unable to find key `%s` in ConfigMap", configmapKey)
		log.Error(err, "unable to find key in ConfigMap", "key", configmapKey)
		return corev1.ConfigMap{}, Configuration{}, err
	}

	var config Configuration
	if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
		log.Error(err, "unable to read config as YAML")
		return corev1.ConfigMap{}, Configuration{}, err
	}
	return configmap, config, nil
}

func (r *IngressReconciler) setConfigMapConfiguration(ctx context.Context, log logr.Logger, configmap corev1.ConfigMap, config Configuration) error {
	// Push updated changesv
	var configStr string
	if configBytes, err := yaml.Marshal(config); err == nil {
		configStr = string(configBytes)
	} else {
		log.Error(err, "unable to marshal config to ConfigMap", "key", configmapKey)
		return err
	}
	configmap.Data[configmapKey] = configStr
	return r.Update(ctx, &configmap)
}

func (r *IngressReconciler) configureCloudflare(log logr.Logger, ctx context.Context, ingress *networkingv1.Ingress, fqdn string, listOpts []client.ListOption, cleanup bool) error {
	var config Configuration
	var configmap corev1.ConfigMap
	var err error

	if configmap, config, err = r.getConfigMapConfiguration(ctx, log, listOpts); err != nil {
		log.Error(err, "unable to get ConfigMap")
		return err
	}
	tunnelDomain := configmap.Labels[tunnelDomainAnnotation]

	var finalIngress []UnvalidatedIngressRule
	if cleanup {
		finalIngress = make([]UnvalidatedIngressRule, 0, len(config.Ingress))
	}
	// Loop through the Ingress rules
	for _, rule := range ingress.Spec.Rules {
		ingressSpecHost := rule.Host

		// Generate fqdn string from Ingress Spec if not provided
		if fqdn == "" {
			ingressHost := strings.Split(ingressSpecHost, ".")[0]
			fqdn = fmt.Sprintf("%s.%s", ingressHost, tunnelDomain)
			log.Info("using default domain value", "domain", tunnelDomain)
		}
		log.Info("setting fqdn", "fqdn", fqdn)

		// Find if the host already exists in config. If so, modify
		found := false
		for i, v := range config.Ingress {
			if cleanup {
				if v.Hostname != fqdn {
					finalIngress = append(finalIngress, v)
				}
			} else if v.Hostname == fqdn {
				log.Info("found existing ingress for host, modifying the service", "service", ingressSpecHost)
				config.Ingress[i].Service = ingressSpecHost
				found = true
				break
			}
		}

		// Else add a new entry
		if !cleanup && !found {
			log.Info("adding ingress for host to point to service", "service", ingressSpecHost)
			config.Ingress = append(config.Ingress, UnvalidatedIngressRule{
				Hostname: fqdn,
				Service:  ingressSpecHost,
			})
		}
	}

	if cleanup {
		if len(finalIngress) > 0 {
			config.Ingress = finalIngress
		} else {
			config.Ingress = nil
			log.Info("nothing left, setting config to nil")
		}
	}
	return r.setConfigMapConfiguration(ctx, log, configmap, config)
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Complete(r)
}
