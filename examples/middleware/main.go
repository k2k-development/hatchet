package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client"
	"github.com/hatchet-dev/hatchet/pkg/cmdutils"
	"github.com/hatchet-dev/hatchet/pkg/worker"
	"github.com/joho/godotenv"
)

type userCreateEvent struct {
	Username string            `json:"username"`
	UserID   string            `json:"user_id"`
	Data     map[string]string `json:"data"`
}

type stepOneOutput struct {
	Message string `json:"message"`
}

func main() {
	err := godotenv.Load()
	if err != nil {
		panic(err)
	}

	events := make(chan string, 50)
	if err := run(cmdutils.InterruptChan(), events); err != nil {
		panic(err)
	}
}

func run(ch <-chan interface{}, events chan<- string) error {
	c, err := client.New()
	if err != nil {
		return fmt.Errorf("error creating client: %w", err)
	}

	// Create a worker. This automatically reads in a TemporalClient from .env and workflow files from the .hatchet
	// directory, but this can be customized with the `worker.WithTemporalClient` and `worker.WithWorkflowFiles` options.
	w, err := worker.NewWorker(
		worker.WithClient(
			c,
		),
	)
	if err != nil {
		return fmt.Errorf("error creating worker: %w", err)
	}

	w.Use(func(ctx context.Context, next func(context.Context) error) error {
		log.Printf("1st-middleware")
		events <- "1st-middleware"
		return next(context.WithValue(ctx, "testkey", "testvalue"))
	})

	w.Use(func(ctx context.Context, next func(context.Context) error) error {
		log.Printf("2nd-middleware")
		events <- "2nd-middleware"

		// time the function duration
		start := time.Now()
		err := next(ctx)
		duration := time.Since(start)
		fmt.Printf("step function took %s\n", duration)
		return err
	})

	testSvc := w.NewService("test")

	testSvc.Use(func(ctx context.Context, next func(context.Context) error) error {
		events <- "svc-middleware"
		return next(context.WithValue(ctx, "svckey", "svcvalue"))
	})

	err = testSvc.On(
		worker.Events("user:create:middleware"),
		&worker.WorkflowJob{
			Name:        "post-user-update",
			Description: "This runs after an update to the user model.",
			Steps: []worker.WorkflowStep{
				worker.Fn(func(ctx context.Context, input *userCreateEvent) (result *stepOneOutput, err error) {
					log.Printf("step-one")
					events <- "step-one"

					// could get from context
					testVal := ctx.Value("testkey").(string)
					events <- testVal
					svcVal := ctx.Value("svckey").(string)
					events <- svcVal

					return &stepOneOutput{
						Message: "Username is: " + input.Username,
					}, nil
				},
				).SetName("step-one"),
				worker.Fn(func(ctx context.Context, input *stepOneOutput) (result *stepOneOutput, err error) {
					log.Printf("step-two")
					events <- "step-two"

					return &stepOneOutput{
						Message: "Above message is: " + input.Message,
					}, nil
				}).SetName("step-two"),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("error registering workflow: %w", err)
	}

	interruptCtx, cancel := cmdutils.InterruptContextFromChan(ch)
	defer cancel()

	go func() {
		err = w.Start(interruptCtx)

		if err != nil {
			panic(err)
		}

		cancel()
	}()

	testEvent := userCreateEvent{
		Username: "echo-test",
		UserID:   "1234",
		Data: map[string]string{
			"test": "test",
		},
	}

	log.Printf("pushing event user:create:middleware")

	// push an event
	err = c.Event().Push(
		context.Background(),
		"user:create:middleware",
		testEvent,
	)
	if err != nil {
		return fmt.Errorf("error pushing event: %w", err)
	}

	for {
		select {
		case <-interruptCtx.Done():
			return nil
		default:
			time.Sleep(time.Second)
		}
	}
}