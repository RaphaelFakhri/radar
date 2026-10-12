package igdebug

import (
	"context"
	"errors"
	domain "github.com/skyhook-io/radar/pkg/igdebug"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestStatusRetriesWithActingUserWhenSharedDiscoveryIsDenied(t *testing.T) {
	shared := fake.NewSimpleClientset()
	shared.Fake.PrependReactor("list", "daemonsets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	acting := fake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gadget", Namespace: "gadget", Labels: map[string]string{"k8s-app": "gadget"}},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "gadget", Image: "ghcr.io/inspektor-gadget/inspektor-gadget:v0.54.1",
		}}}}},
	})
	service := NewService("")
	service.sharedClient = func() kubernetes.Interface { return shared }
	service.clientFromContext = func(context.Context) kubernetes.Interface { return acting }

	status := service.Status(context.Background())

	if status.State != StateInstalled || status.Namespace != "gadget" || status.Version != "v0.54.1" {
		t.Fatalf("status = %#v", status)
	}
}

func TestStartIsQuiescedUntilContextReady(t *testing.T) {
	service := NewService("")
	service.sharedClient = func() kubernetes.Interface { return fake.NewSimpleClientset() }
	service.OnContextSwitch()
	request := domain.Request{Aspect: domain.AspectProcesses}
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrQuiescing) {
		t.Fatalf("Start during transition = %v", err)
	}
	service.OnContextReady()
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Start after transition = %v", err)
	}
}

func TestContextSwitchDoesNotWaitForDiscovery(t *testing.T) {
	client := fake.NewSimpleClientset()
	entered, release := make(chan struct{}), make(chan struct{})
	client.PrependReactor("list", "daemonsets", func(ktesting.Action) (bool, runtime.Object, error) {
		close(entered)
		<-release
		return true, &appsv1.DaemonSetList{}, nil
	})
	service := NewService("")
	service.sharedClient = func() kubernetes.Interface { return client }
	started := make(chan struct{})
	go func() {
		defer close(started)
		_, _ = service.Start(context.Background(), domain.Request{Aspect: domain.AspectProcesses})
	}()
	<-entered
	quiesced := make(chan struct{})
	go func() { service.OnContextSwitch(); close(quiesced) }()
	select {
	case <-quiesced:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("context switch waited for capture discovery")
	}
	close(release)
	<-started
	if _, err := service.Start(context.Background(), domain.Request{Aspect: domain.AspectProcesses}); !errors.Is(err, ErrQuiescing) {
		t.Fatalf("admitted a capture after teardown: %v", err)
	}
}
