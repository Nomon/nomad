package command

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hashicorp/nomad/api"
)

type LogsCommand struct {
	Meta
}

func (l *LogsCommand) Help() string {
	helpText := `
Usage: nomad logs [options] <alloc-id> <task>

TODO

General Options:

  ` + generalOptionsUsage() + `

Logs Specific Options:

  -verbose
    Show full information.

  -job <job-id>
    Use a random allocation from a specified job-id.

  -tail 
	Show the files contents with offsets relative to the end of the file. If no
	offset is given, -n is defaulted to 10.

  -n
	Sets the tail location in best-efforted number of lines relative to the end
	of the file.

  -c
	Sets the tail location in number of bytes relative to the end of the file.
`
	return strings.TrimSpace(helpText)
}

func (l *LogsCommand) Synopsis() string {
	return "Inspect the contents of an allocation directory"
}

func (l *LogsCommand) Run(args []string) int {
	var verbose, job, tail, stderr bool
	var numLines, numBytes int64

	flags := l.Meta.FlagSet("logs-list", FlagSetClient)
	flags.Usage = func() { l.Ui.Output(l.Help()) }
	flags.BoolVar(&verbose, "verbose", false, "")
	flags.BoolVar(&job, "job", false, "")
	flags.BoolVar(&tail, "tail", false, "")
	flags.BoolVar(&stderr, "stderr", false, "")
	flags.Int64Var(&numLines, "n", -1, "")
	flags.Int64Var(&numBytes, "c", -1, "")

	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()

	if len(args) < 2 {
		if job {
			l.Ui.Error("job ID and task name required")
		} else {
			l.Ui.Error("allocation ID and task name required")
		}

		return 1
	}

	task := args[1]
	if task == "" {
		l.Ui.Error("task name required")
		return 1
	}

	client, err := l.Meta.Client()
	if err != nil {
		l.Ui.Error(fmt.Sprintf("Error initializing client: %v", err))
		return 1
	}

	// If -job is specified, use random allocation, otherwise use provided allocation
	allocID := args[0]
	if job {
		allocID, err = getRandomJobAlloc(client, args[0])
		if err != nil {
			l.Ui.Error(fmt.Sprintf("Error fetching allocations: %v", err))
			return 1
		}
	}

	// Truncate the id unless full length is requested
	length := shortId
	if verbose {
		length = fullId
	}
	// Query the allocation info
	if len(allocID) == 1 {
		l.Ui.Error(fmt.Sprintf("Alloc ID must contain at least two characters."))
		return 1
	}
	if len(allocID)%2 == 1 {
		// Identifiers must be of even length, so we strip off the last byte
		// to provide a consistent user experience.
		allocID = allocID[:len(allocID)-1]
	}

	allocs, _, err := client.Allocations().PrefixList(allocID)
	if err != nil {
		l.Ui.Error(fmt.Sprintf("Error querying allocation: %v", err))
		return 1
	}
	if len(allocs) == 0 {
		l.Ui.Error(fmt.Sprintf("No allocation(s) with prefix or id %q found", allocID))
		return 1
	}
	if len(allocs) > 1 {
		// Format the allocs
		out := make([]string, len(allocs)+1)
		out[0] = "ID|Eval ID|Job ID|Task Group|Desired Status|Client Status"
		for i, alloc := range allocs {
			out[i+1] = fmt.Sprintf("%s|%s|%s|%s|%s|%s",
				limit(alloc.ID, length),
				limit(alloc.EvalID, length),
				alloc.JobID,
				alloc.TaskGroup,
				alloc.DesiredStatus,
				alloc.ClientStatus,
			)
		}
		l.Ui.Output(fmt.Sprintf("Prefix matched multiple allocations\n\n%s", formatList(out)))
		return 0
	}
	// Prefix lookup matched a single allocation
	alloc, _, err := client.Allocations().Info(allocs[0].ID, nil)
	if err != nil {
		l.Ui.Error(fmt.Sprintf("Error querying allocation: %s", err))
		return 1
	}

	logType := "stdout"
	if stderr {
		logType = "stderr"
	}

	// We have a file, output it.
	var r io.ReadCloser
	var readErr error
	if !tail {
		r, readErr = l.followFile(client, alloc, task, logType, api.OriginStart, 0)
		if readErr != nil {
			readErr = fmt.Errorf("Error reading file: %v", readErr)
		}
	} else {
		// Parse the offset
		var offset int64 = defaultTailLines * bytesToLines

		if nLines, nBytes := numLines != -1, numBytes != -1; nLines && nBytes {
			l.Ui.Error("Both -n and -c set")
			return 1
		} else if nLines {
			offset = numLines * bytesToLines
		} else if nBytes {
			offset = numBytes
		} else {
			numLines = defaultTailLines
		}

		r, readErr = l.followFile(client, alloc, task, logType, api.OriginEnd, offset)

		// If numLines is set, wrap the reader
		if numLines != -1 {
			r = NewLineLimitReader(r, int(numLines), int(numLines*bytesToLines))
		}

		if readErr != nil {
			readErr = fmt.Errorf("Error tailing file: %v", readErr)
		}
	}

	defer r.Close()
	if readErr != nil {
		l.Ui.Error(readErr.Error())
		return 1
	}

	io.Copy(os.Stdout, r)
	return 0
}

// followFile outputs the contents of the file to stdout relative to the end of
// the file.
func (l *LogsCommand) followFile(client *api.Client, alloc *api.Allocation,
	task, logType, origin string, offset int64) (io.ReadCloser, error) {

	cancel := make(chan struct{})
	frames, _, err := client.AllocFS().Logs(alloc, task, logType, origin, offset, cancel, nil)
	if err != nil {
		return nil, err
	}
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	// Create a reader
	var r io.ReadCloser
	frameReader := api.NewFrameReader(frames, cancel)
	r = frameReader

	go func() {
		<-signalCh

		// End the streaming
		r.Close()

		// Output the last offset
		l.Ui.Output(fmt.Sprintf("\nLast outputted offset (bytes): %d", frameReader.Offset()))
	}()

	return r, nil
}
