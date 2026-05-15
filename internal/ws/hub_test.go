package ws

import "testing"

func TestHubBroadcastSendsToReadyClient(t *testing.T) {
	hub := NewHub()
	client := &Client{Send: make(chan []byte, 1)}
	hub.registerClient(client)

	hub.broadcast([]byte("tick"))

	if !hub.Clients[client] {
		t.Fatal("expected ready client to remain registered")
	}

	got := <-client.Send
	if string(got) != "tick" {
		t.Fatalf("broadcast payload = %q, want %q", got, "tick")
	}
}

func TestHubBroadcastDropsSlowClient(t *testing.T) {
	hub := NewHub()
	client := &Client{Send: make(chan []byte, 1)}
	client.Send <- []byte("queued")
	hub.registerClient(client)

	hub.broadcast([]byte("latest"))

	if hub.Clients[client] {
		t.Fatal("expected slow client to be removed")
	}

	got, ok := <-client.Send
	if !ok {
		t.Fatal("expected queued message to remain readable before channel close is observed")
	}
	if string(got) != "queued" {
		t.Fatalf("queued payload = %q, want %q", got, "queued")
	}

	_, ok = <-client.Send
	if ok {
		t.Fatal("expected slow client send channel to be closed")
	}
}

func TestHubUnregisterRemovesClientAndClosesSend(t *testing.T) {
	hub := NewHub()
	client := &Client{Send: make(chan []byte)}
	hub.registerClient(client)

	hub.unregisterClient(client)

	if hub.Clients[client] {
		t.Fatal("expected unregistered client to be removed")
	}

	_, ok := <-client.Send
	if ok {
		t.Fatal("expected unregistered client send channel to be closed")
	}

	hub.unregisterClient(client)
}
