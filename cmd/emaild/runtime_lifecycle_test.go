package main

import (
	"reflect"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func TestActivateMailboxServiceHTTPOnlyRegistersWithoutWorkers(t *testing.T) {
	mailbox := store.Mailbox{ID: "mailbox-1", Name: "primary"}
	var calls []string
	started := activateMailboxService(
		mailbox,
		false,
		func(store.Mailbox) *service.Service {
			calls = append(calls, "build")
			return &service.Service{MailboxID: mailbox.ID, MailboxName: mailbox.Name}
		},
		func(name string, _ *service.Service) bool {
			calls = append(calls, "register:"+name)
			return true
		},
		func(*service.Service) { calls = append(calls, "queues") },
		func(string, *service.Service) bool {
			calls = append(calls, "poller")
			return true
		},
		func(string, string) { calls = append(calls, "running") },
	)
	if started {
		t.Fatal("HTTP-only activation must not report a poller start")
	}
	if want := []string{"build", "register:primary"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("HTTP-only calls = %v, want %v", calls, want)
	}
}

func TestActivateMailboxServiceStartsWorkersOnlyAfterRegistration(t *testing.T) {
	mailbox := store.Mailbox{ID: "mailbox-1", Name: "primary"}
	var calls []string
	started := activateMailboxService(
		mailbox,
		true,
		func(store.Mailbox) *service.Service {
			calls = append(calls, "build")
			return &service.Service{MailboxID: mailbox.ID, MailboxName: mailbox.Name}
		},
		func(name string, _ *service.Service) bool {
			calls = append(calls, "register:"+name)
			return true
		},
		func(*service.Service) { calls = append(calls, "queues") },
		func(name string, _ *service.Service) bool {
			calls = append(calls, "poller:"+name)
			return true
		},
		func(name, id string) { calls = append(calls, "running:"+name+":"+id) },
	)
	if !started {
		t.Fatal("worker activation should report its poller start")
	}
	want := []string{"build", "register:primary", "queues", "poller:primary", "running:primary:mailbox-1"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("worker calls = %v, want %v", calls, want)
	}
}

func TestActivateMailboxServiceDoesNotStartWorkersWhenRegistrationLosesRace(t *testing.T) {
	mailbox := store.Mailbox{ID: "mailbox-1", Name: "primary"}
	queues := 0
	pollers := 0
	started := activateMailboxService(
		mailbox,
		true,
		func(store.Mailbox) *service.Service { return &service.Service{} },
		func(string, *service.Service) bool { return false },
		func(*service.Service) { queues++ },
		func(string, *service.Service) bool { pollers++; return true },
		func(string, string) {},
	)
	if started || queues != 0 || pollers != 0 {
		t.Fatalf("lost registration started=%v queues=%d pollers=%d", started, queues, pollers)
	}
}

func TestStartConfiguredQueueWorkersHonorsHTTPOnlyMode(t *testing.T) {
	services := map[string]*service.Service{"primary": &service.Service{MailboxName: "primary"}}
	starts := 0
	startConfiguredQueueWorkers(false, services, func(*service.Service) { starts++ })
	if starts != 0 {
		t.Fatalf("HTTP-only startup launched %d queue worker sets", starts)
	}
	startConfiguredQueueWorkers(true, services, func(*service.Service) { starts++ })
	if starts != 1 {
		t.Fatalf("worker startup launched %d queue worker sets, want 1", starts)
	}
}

func TestAllMailboxesWorkerCanIdleUntilFirstMailbox(t *testing.T) {
	if rejectsEmptyRuntime(true, runtimeOptions{allMailboxes: true, withPollers: true}) {
		t.Fatal("all-mailboxes worker must stay alive for the mailbox reconciler")
	}
	if !rejectsEmptyRuntime(true, runtimeOptions{mailbox: "missing", withPollers: true}) {
		t.Fatal("single-mailbox worker must still reject an empty database")
	}
	if rejectsEmptyRuntime(true, runtimeOptions{allMailboxes: true, withHTTP: true}) {
		t.Fatal("HTTP onboarding runtime must allow an empty database")
	}
}

func TestLazyMailboxLoadingIsLimitedToAPIOnlyAllMailboxes(t *testing.T) {
	if !enablesLazyMailboxLoading(runtimeOptions{withHTTP: true, allMailboxes: true}) {
		t.Fatal("API-only all-mailboxes runtime must load mailboxes discovered by other instances")
	}
	for _, opts := range []runtimeOptions{
		{withHTTP: true, withPollers: true, allMailboxes: true},
		{withHTTP: true, mailbox: "primary"},
		{withPollers: true, allMailboxes: true},
	} {
		if enablesLazyMailboxLoading(opts) {
			t.Fatalf("unexpected lazy mailbox loading for options %+v", opts)
		}
	}
}
