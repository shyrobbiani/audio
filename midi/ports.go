package midi

/*
A Port has go channels for reading / writing MIDI data
and may read / write from underlying system MIDI streams via C.
There are input ports (for output streams) and output ports
(for input streams). A Port is to represent the physical
MIDI in and MIDI out ports of devices, not the file streams
that the OS uses to transfer data to them.
*/

// #cgo LDFLAGS: -lportmidi
// #include <portmidi.h>
import "C"
import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

type Port interface {
	Open() error
	Close() error
	IsOpen() bool
	Run()
}

func makePortMidiError(errNum C.PmError) error {
	msg := C.GoString(C.Pm_GetErrorText(errNum))
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}

// Implements Port, prentinding to be a system port for transposed values.
type FakePort struct {
	isOpen      bool
	events      chan Event
	IsInputPort bool
}

func (t *FakePort) Open() error {
	t.isOpen = true
	t.events = make(chan Event, BufferSize)
	return nil
}

func (t *FakePort) Close() error {
	close(t.events)
	t.isOpen = false
	return nil
}

func (t FakePort) IsOpen() bool {
	return t.isOpen
}

func (t FakePort) Run() {
	// Do nothing, Run is handled by the Transposer.
}

// Implements Port, abstracting a system MIDI stream as a port.
type SystemPort struct {
	isOpen bool
	id     int
	stream unsafe.Pointer
	events chan Event
	stop   chan bool
}

type SystemInPort struct {
	SystemPort
}

type SystemOutPort struct {
	SystemPort
}

func (s *SystemOutPort) Open() error {
	if s.isOpen && s.stream == nil {
		return errors.New("Underlying portmidi port is already opened, " +
			"but stream is not connected to this SystemPort.")
	}
	if s.id == -1 || s.isOpen { // Fake port or opened already, ignore.
		return nil
	}
	errNum := C.Pm_OpenInput(&(s.stream), C.PmDeviceID(s.id),
		nil, C.int32_t(512), nil, nil)

	if errNum == 0 {
		s.isOpen = true
	}
	return makePortMidiError(errNum)
}

func (s *SystemInPort) Open() error {
	if s.isOpen && s.stream == nil {
		return errors.New("Underlying portmidi port is already opened, " +
			"but stream is not connected to this SystemPort.")
	}
	if s.id == -1 || s.isOpen { // Fake port or opened already, ignore.
		return nil
	}
	// The input / output naming LOOKS backwards, but we're opening a
	// portmidi "output stream" for input Ports and vice versa.
	errNum := C.Pm_OpenOutput(&(s.stream), C.PmDeviceID(s.id), nil, C.int32_t(512), nil, nil, 0)
	if errNum == 0 {
		s.isOpen = true
	}
	return makePortMidiError(errNum)
}

func (s *SystemPort) Close() error {
	if s.isOpen {
		s.isOpen = false
		s.stop <- true
		errNum := C.Pm_Close(s.stream)
		close(s.events)
		return makePortMidiError(errNum)
	}
	return nil
}

func (s SystemPort) IsOpen() bool {
	return s.isOpen
}

// TODO: Event should be an interface.
func (s SystemInPort) Run() {
	if debug {
		fmt.Println("SystemInPort", s.id, "RunInPort()")
	}
	// A device's input port receives data - write to the port.
	for {
		select {
		case e := <-s.events:
			s.writeEvent(e)
		case <-s.stop:
			return
		}
	}
}

func (s SystemOutPort) Run() {
	if debug {
		fmt.Println("SystemOutPort", s.id, "RunOutputPort()")
	}
	// A device's output port sends data to something else - read from the port.
	for {
		select {
		case <-s.stop:
			return
		default:
			dataAvailable, err := s.poll()
			if err != nil {
				panic(err)
			}
			if dataAvailable == false {
				time.Sleep(1 * time.Millisecond)
				continue
			}
			m, err := s.readEvent()
			if err != nil {
				continue // TODO: This is questionable error handling.
			}
			if debug {
				fmt.Println("SystemPort RunOutputPort()", s.id, m)
			}
			switch m.Command {
			case NOTE_ON:
				fmt.Println("note on")
				s.events <- NoteOn{m.Channel, m.Data1, m.Data2}
				fmt.Println("sent note on")
			case NOTE_OFF:
				// A NoteOn with velocity 0 (Data2) is arguably a Note Off.
				s.events <- NoteOff{m.Channel, m.Data1, 0}
			case CONTROL_CHANGE:
				name, ok := ControlChangeNames[m.Data1]
				if !ok {
					name = "Unknown"
				}
				s.events <- ControlChange{m.Channel, m.Data1, m.Data2, name}
			default:
				s.events <- m
			}
		}
	}
}

func (s SystemOutPort) poll() (bool, error) {
	if s.stream == nil {
		return false, errors.New("No input stream set.")
	}
	if s.IsOpen() == false {
		return false, errors.New("Port is not open.")
	}
	dataAvailable, err := C.Pm_Poll(s.stream)
	if err != nil {
		return false, err // Tried to read data, failed.
	}
	if dataAvailable > 0 {
		return true, nil // Data available.
	}
	return false, nil // No data available.
}

// TODO: Fulfill io.Reader and io.Writer interfaces
func (s SystemOutPort) readEvent() (Message, error) {
	var buffer C.PmEvent
	// Only read one event at a time.
	eventsRead := int(C.Pm_Read(s.stream, &buffer, C.int32_t(1)))
	m := Message{}
	if eventsRead > 0 {
		status := int(buffer.message) & 0xFF
		m.Channel = int(status & 0x0F)
		m.Command = int(status & 0xF0)
		m.Data1 = int((buffer.message >> 8) & 0xFF)
		m.Data2 = int((buffer.message >> 16) & 0xFF)
	}
	return m, nil
}

func (s *SystemInPort) writeEvent(event Event) error {
	message := event.ToRawMessage()
	if debug {
		fmt.Printf("%b\n", message)
	}
	buffer := C.PmEvent{C.PmMessage(message), 0}
	err := C.Pm_Write(s.stream, &buffer, C.int32_t(1))
	return makePortMidiError(err)
}

// This is not the method you're looking for. Avoid it.
// It bypasses MIDI-message-type-specific channels in order to
// broadcast many disparate types of messages to hardware where the order of
// message arrival matters greatly. It exists to handle an edge case on one
// piece of hardware and its peculiar internal protocols.
func (s *SystemInPort) WriteRawEvent(m Message) error {
	return s.writeEvent(m) // TODO(aoeu): Assert this works without bit bashing.
}
