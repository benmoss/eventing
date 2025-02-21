/*
Copyright 2020 The Knative Authors

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

package mttrigger

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"knative.dev/eventing/pkg/apis/eventing"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	messagingv1 "knative.dev/eventing/pkg/apis/messaging/v1"
	clientset "knative.dev/eventing/pkg/client/clientset/versioned"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1"
	messaginglisters "knative.dev/eventing/pkg/client/listers/messaging/v1"
	"knative.dev/eventing/pkg/duck"
	"knative.dev/eventing/pkg/reconciler/broker/resources"
	"knative.dev/eventing/pkg/reconciler/sugar/trigger/path"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/network"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
	"knative.dev/pkg/system"
)

var brokerGVK = eventingv1.SchemeGroupVersion.WithKind("Broker")

const (
	// Name of the corev1.Events emitted from the Trigger reconciliation process.
	subscriptionDeleteFailed = "SubscriptionDeleteFailed"
	subscriptionCreateFailed = "SubscriptionCreateFailed"
	subscriptionGetFailed    = "SubscriptionGetFailed"
)

type Reconciler struct {
	eventingClientSet clientset.Interface
	dynamicClientSet  dynamic.Interface

	// listers index properties about resources
	subscriptionLister messaginglisters.SubscriptionLister
	brokerLister       eventinglisters.BrokerLister
	triggerLister      eventinglisters.TriggerLister
	configmapLister    corev1listers.ConfigMapLister

	// Dynamic tracker to track Sources. In particular, it tracks the dependency between Triggers and Sources.
	sourceTracker duck.ListableTracker

	// Dynamic tracker to track AddressableTypes. In particular, it tracks Trigger subscribers.
	uriResolver *resolver.URIResolver
	impl        *controller.Impl
}

func (r *Reconciler) ReconcileKind(ctx context.Context, t *eventingv1.Trigger) pkgreconciler.Event {
	logging.FromContext(ctx).Infow("Reconciling", zap.Any("Trigger", t))
	t.Status.InitializeConditions()

	if t.DeletionTimestamp != nil {
		// Everything is cleaned up by the garbage collector.
		return nil
	}

	b, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)
	if err != nil {
		if apierrs.IsNotFound(err) {
			logging.FromContext(ctx).Errorw(fmt.Sprintf("Trigger %s/%s has no broker %q", t.Namespace, t.Name, t.Spec.Broker))
			t.Status.MarkBrokerFailed("BrokerDoesNotExist", "Broker %q does not exist", t.Spec.Broker)
			// Ok to return nil here. Once the Broker comes available, or Trigger changes, we get requeued.
			return nil
		} else {
			t.Status.MarkBrokerFailed("FailedToGetBroker", "Failed to get broker %q : %s", t.Spec.Broker, err)
			return err
		}
	}

	// If it's not my brokerclass, ignore
	if b.Annotations[eventing.BrokerClassKey] != eventing.MTChannelBrokerClassValue {
		logging.FromContext(ctx).Infof("Ignoring trigger %s/%s", t.Namespace, t.Name)
		return nil
	}
	t.Status.PropagateBrokerCondition(b.Status.GetTopLevelCondition())

	// If Broker is not ready, we're done, but once it becomes ready, we'll get requeued.
	if !b.IsReady() {
		logging.FromContext(ctx).Errorw("Broker is not ready", zap.Any("Broker", b))
		return nil
	}

	brokerTrigger, err := getBrokerChannelRef(b)
	if err != nil {
		t.Status.MarkBrokerFailed("MissingBrokerChannel", "Failed to get broker %q annotations: %s", t.Spec.Broker, err)
		return fmt.Errorf("failed to find Broker's Trigger channel: %s", err)
	}
	if t.Spec.Subscriber.Ref != nil && t.Spec.Subscriber.Ref.Namespace == "" {
		// To call URIFromDestinationV1(ctx context.Context, dest v1.Destination, parent interface{}), dest.Ref must have a Namespace
		// If Subscriber.Ref.Namespace is nil, We will use the Namespace of Trigger as the Namespace of dest.Ref
		t.Spec.Subscriber.Ref.Namespace = t.GetNamespace()
	}

	subscriberURI, err := r.uriResolver.URIFromDestinationV1(ctx, t.Spec.Subscriber, b)
	if err != nil {
		logging.FromContext(ctx).Errorw("Unable to get the Subscriber's URI", zap.Error(err))
		t.Status.MarkSubscriberResolvedFailed("Unable to get the Subscriber's URI", "%v", err)
		t.Status.SubscriberURI = nil
		return err
	}
	t.Status.SubscriberURI = subscriberURI
	t.Status.MarkSubscriberResolvedSucceeded()

	if err := r.resolveDeadLetterSink(ctx, b, t); err != nil {
		return err
	}

	sub, err := r.subscribeToBrokerChannel(ctx, b, t, brokerTrigger)
	if err != nil {
		logging.FromContext(ctx).Errorw("Unable to Subscribe", zap.Error(err))
		t.Status.MarkNotSubscribed("NotSubscribed", "%v", err)
		return err
	}
	t.Status.PropagateSubscriptionCondition(sub.Status.GetTopLevelCondition())

	if err := r.checkDependencyAnnotation(ctx, t); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) resolveDeadLetterSink(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger) error {
	// resolve the trigger's dls first, fall back to the broker's
	if t.Spec.Delivery != nil && t.Spec.Delivery.DeadLetterSink != nil {
		deadLetterSinkURI, err := r.uriResolver.URIFromDestinationV1(ctx, *t.Spec.Delivery.DeadLetterSink, t)
		if err != nil {
			t.Status.DeadLetterSinkURI = nil
			logging.FromContext(ctx).Errorw("Unable to get the dead letter sink's URI", zap.Error(err))
			t.Status.MarkDeadLetterSinkResolvedFailed("Unable to get the dead letter sink's URI", "%v", err)
			return err
		}

		t.Status.DeadLetterSinkURI = deadLetterSinkURI
		t.Status.MarkDeadLetterSinkResolvedSucceeded()
		// In case there is no DLS defined in the Trigger Spec, fallback to Broker's
	} else if b.Spec.Delivery != nil && b.Spec.Delivery.DeadLetterSink != nil {
		if b.Status.DeadLetterSinkURI != nil {
			t.Status.DeadLetterSinkURI = b.Status.DeadLetterSinkURI
			t.Status.MarkDeadLetterSinkResolvedSucceeded()
		} else {
			t.Status.DeadLetterSinkURI = nil
			t.Status.MarkDeadLetterSinkResolvedFailed(fmt.Sprintf("Broker %s didn't set status.deadLetterSinkURI", b.Name), "")
			return fmt.Errorf("broker %s didn't set status.deadLetterSinkURI", b.Name)
		}
	} else {
		// There is no DLS defined in nither Trigger nor the Broker
		t.Status.DeadLetterSinkURI = nil
		t.Status.MarkDeadLetterSinkNotConfigured()
	}

	return nil
}

// subscribeToBrokerChannel subscribes service 'svc' to the Broker's channels.
func (r *Reconciler) subscribeToBrokerChannel(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger, brokerTrigger *corev1.ObjectReference) (*messagingv1.Subscription, error) {
	recorder := controller.GetEventRecorder(ctx)
	uri := &apis.URL{
		Scheme: "http",
		Host:   network.GetServiceHostname("broker-filter", system.Namespace()),
		Path:   path.Generate(t),
	}
	// Note that we have to hard code the brokerGKV stuff as sometimes typemeta is not
	// filled in. So instead of b.TypeMeta.Kind and b.TypeMeta.APIVersion, we have to
	// do it this way.
	brokerObjRef := &corev1.ObjectReference{
		Kind:       brokerGVK.Kind,
		APIVersion: brokerGVK.GroupVersion().String(),
		Name:       b.Name,
		Namespace:  b.Namespace,
	}

	delivery := t.Spec.Delivery
	if delivery == nil {
		delivery = b.Spec.Delivery
	}

	expected := resources.NewSubscription(t, brokerTrigger, brokerObjRef, uri, delivery)

	sub, err := r.subscriptionLister.Subscriptions(t.Namespace).Get(expected.Name)
	// If the resource doesn't exist, we'll create it.
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Infow("Creating subscription", zap.Error(err))
		sub, err = r.eventingClientSet.MessagingV1().Subscriptions(t.Namespace).Create(ctx, expected, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return sub, nil
	} else if err != nil {
		logging.FromContext(ctx).Errorw("Failed to get subscription", zap.Error(err))
		recorder.Eventf(t, corev1.EventTypeWarning, subscriptionGetFailed, "Getting the Trigger's Subscription failed: %v", err)
		return nil, err
	} else if !metav1.IsControlledBy(sub, t) {
		t.Status.MarkNotSubscribed("SubscriptionNotOwnedByTrigger", "trigger %q does not own subscription %q", t.Name, sub.Name)
		return nil, fmt.Errorf("trigger %q does not own subscription %q", t.Name, sub.Name)
	} else if sub, err = r.reconcileSubscription(ctx, t, expected, sub); err != nil {
		logging.FromContext(ctx).Errorw("Failed to reconcile subscription", zap.Error(err))
		return sub, err
	}

	return sub, nil
}

func (r *Reconciler) reconcileSubscription(ctx context.Context, t *eventingv1.Trigger, expected, actual *messagingv1.Subscription) (*messagingv1.Subscription, error) {
	// Update Subscription if it has changed.
	if equality.Semantic.DeepDerivative(expected.Spec, actual.Spec) {
		return actual, nil
	}
	recorder := controller.GetEventRecorder(ctx)
	logging.FromContext(ctx).Infow("Differing Subscription", zap.Any("expected", expected.Spec), zap.Any("actual", actual.Spec))

	// Given that spec.channel is immutable, we cannot just update the Subscription. We delete
	// it and re-create it instead.
	logging.FromContext(ctx).Infow("Deleting subscription", zap.String("namespace", actual.Namespace), zap.String("name", actual.Name))
	err := r.eventingClientSet.MessagingV1().Subscriptions(t.Namespace).Delete(ctx, actual.Name, metav1.DeleteOptions{})
	if err != nil {
		logging.FromContext(ctx).Info("Cannot delete subscription", zap.Error(err))
		recorder.Eventf(t, corev1.EventTypeWarning, subscriptionDeleteFailed, "Delete Trigger's subscription failed: %v", err)
		return nil, err
	}
	logging.FromContext(ctx).Info("Creating subscription")
	newSub, err := r.eventingClientSet.MessagingV1().Subscriptions(t.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if err != nil {
		logging.FromContext(ctx).Infow("Cannot create subscription", zap.Error(err))
		recorder.Eventf(t, corev1.EventTypeWarning, subscriptionCreateFailed, "Create Trigger's subscription failed: %v", err)
		return nil, err
	}
	return newSub, nil
}

func (r *Reconciler) checkDependencyAnnotation(ctx context.Context, t *eventingv1.Trigger) error {
	if dependencyAnnotation, ok := t.GetAnnotations()[eventingv1.DependencyAnnotation]; ok {
		dependencyObjRef, err := eventingv1.GetObjRefFromDependencyAnnotation(dependencyAnnotation)
		if err != nil {
			t.Status.MarkDependencyFailed("ReferenceError", "Unable to unmarshal objectReference from dependency annotation of trigger: %v", err)
			return fmt.Errorf("getting object ref from dependency annotation %q: %v", dependencyAnnotation, err)
		}
		trackSource := r.sourceTracker.TrackInNamespace(ctx, t)
		// Trigger and its dependent source are in the same namespace, we already did the validation in the webhook.
		if err := trackSource(dependencyObjRef); err != nil {
			return fmt.Errorf("tracking dependency: %v", err)
		}
		if err := r.propagateDependencyReadiness(ctx, t, dependencyObjRef); err != nil {
			return fmt.Errorf("propagating dependency readiness: %v", err)
		}
	} else {
		t.Status.MarkDependencySucceeded()
	}
	return nil
}

func (r *Reconciler) propagateDependencyReadiness(ctx context.Context, t *eventingv1.Trigger, dependencyObjRef corev1.ObjectReference) error {
	lister, err := r.sourceTracker.ListerFor(dependencyObjRef)
	if err != nil {
		t.Status.MarkDependencyUnknown("ListerDoesNotExist", "Failed to retrieve lister: %v", err)
		return fmt.Errorf("retrieving lister: %v", err)
	}
	dependencyObj, err := lister.ByNamespace(t.GetNamespace()).Get(dependencyObjRef.Name)
	if err != nil {
		if apierrs.IsNotFound(err) {
			t.Status.MarkDependencyFailed("DependencyDoesNotExist", "Dependency does not exist: %v", err)
		} else {
			t.Status.MarkDependencyUnknown("DependencyGetFailed", "Failed to get dependency: %v", err)
		}
		return fmt.Errorf("getting the dependency: %v", err)
	}
	dependency := dependencyObj.(*duckv1.Source)

	// The dependency hasn't yet reconciled our latest changes to
	// its desired state, so its conditions are outdated.
	if dependency.GetGeneration() != dependency.Status.ObservedGeneration {
		logging.FromContext(ctx).Infow("The ObjectMeta Generation of dependency is not equal to the observedGeneration of status",
			zap.Any("objectMetaGeneration", dependency.GetGeneration()),
			zap.Any("statusObservedGeneration", dependency.Status.ObservedGeneration))
		t.Status.MarkDependencyUnknown("GenerationNotEqual", "The dependency's metadata.generation, %q, is not equal to its status.observedGeneration, %q.", dependency.GetGeneration(), dependency.Status.ObservedGeneration)
		return nil
	}
	t.Status.PropagateDependencyStatus(dependency)
	return nil
}

func getBrokerChannelRef(b *eventingv1.Broker) (*corev1.ObjectReference, error) {
	if b.Status.Annotations != nil {
		ref := &corev1.ObjectReference{
			Kind:       b.Status.Annotations[eventing.BrokerChannelKindStatusAnnotationKey],
			APIVersion: b.Status.Annotations[eventing.BrokerChannelAPIVersionStatusAnnotationKey],
			Name:       b.Status.Annotations[eventing.BrokerChannelNameStatusAnnotationKey],
			Namespace:  b.Namespace,
		}
		if ref.Kind != "" && ref.APIVersion != "" && ref.Name != "" && ref.Namespace != "" {
			return ref, nil
		}
	}
	return nil, errors.New("Broker.Status.Annotations nil or missing values")
}
