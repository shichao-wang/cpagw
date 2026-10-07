package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

const maxPromptHandoffKeys = 4096

type promptOutcome struct {
	value any
	err   error
}

type promptWaiter struct {
	id     uint64
	result chan promptOutcome
}

type promptSession struct {
	program   *tea.Program
	ready     chan struct{}
	runDone   chan struct{}
	closeDone chan struct{}
	in        io.Reader
	out       io.Writer
	ctx       context.Context

	mu          sync.Mutex
	waiter      *promptWaiter
	nextID      uint64
	started     bool
	closed      bool
	canceled    bool
	runErr      error
	requestLock chan struct{}
}

func newPromptSession(ctx context.Context, in io.Reader, out io.Writer) *promptSession {
	return &promptSession{
		ctx: ctx, in: in, out: out,
		ready: make(chan struct{}), runDone: make(chan struct{}), closeDone: make(chan struct{}),
		requestLock: make(chan struct{}, 1),
	}
}

func (s *promptSession) ensureStarted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.canceled {
		s.mu.Unlock()
		return context.Canceled
	}
	if !s.started {
		s.started = true
		model := &promptSessionModel{ready: s.ready, finish: s.finish, cancel: s.cancel}
		input := s.in
		var ownedInput *promptInput
		if file, ok := input.(*os.File); ok {
			ownedInput = newPromptInput(file)
			input = ownedInput
		}
		s.program = tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(s.out),
			tea.WithContext(s.ctx), tea.WithoutSignalHandler())
		program := s.program
		go func() {
			_, err := program.Run()
			if ownedInput != nil {
				_ = ownedInput.Close()
			}
			s.mu.Lock()
			s.runErr = err
			waiter := s.waiter
			s.waiter = nil
			s.mu.Unlock()
			if waiter != nil {
				waiter.result <- promptOutcome{err: s.exitError()}
			}
			close(s.runDone)
		}()
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closeDone:
		return context.Canceled
	case <-s.runDone:
		return s.exitError()
	case <-s.ready:
		return nil
	}
}

func (s *promptSession) exitError() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runErr != nil && !errors.Is(s.runErr, tea.ErrInterrupted) && !errors.Is(s.runErr, tea.ErrProgramKilled) {
		return s.runErr
	}
	return context.Canceled
}

func (s *promptSession) prompt(ctx context.Context, form *huh.Form) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closeDone:
		return nil, context.Canceled
	case s.requestLock <- struct{}{}:
	}
	defer func() { <-s.requestLock }()
	if err := s.ensureStarted(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.closed || s.canceled {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	s.nextID++
	waiter := &promptWaiter{id: s.nextID, result: make(chan promptOutcome, 1)}
	s.waiter = waiter
	program := s.program
	s.mu.Unlock()
	program.Send(promptFormMsg{id: waiter.id, form: form})
	select {
	case <-ctx.Done():
		_ = s.close()
		return nil, ctx.Err()
	case <-s.closeDone:
		return nil, context.Canceled
	case result := <-waiter.result:
		return result.value, result.err
	case <-s.runDone:
		select {
		case result := <-waiter.result:
			return result.value, result.err
		default:
			return nil, s.exitError()
		}
	}
}

func (s *promptSession) finish(id uint64, outcome promptOutcome) {
	s.mu.Lock()
	waiter := s.waiter
	if waiter == nil || waiter.id != id {
		s.mu.Unlock()
		return
	}
	s.waiter = nil
	s.mu.Unlock()
	waiter.result <- outcome
}

func (s *promptSession) cancel() {
	s.mu.Lock()
	s.canceled = true
	waiter := s.waiter
	s.waiter = nil
	s.mu.Unlock()
	if waiter != nil {
		waiter.result <- promptOutcome{err: context.Canceled}
	}
	// 此回调在 UI 线程内执行；由 Model 返回 Quit，不能同步向自己 Send。
}

func (s *promptSession) close() error {
	s.mu.Lock()
	alreadyClosed := s.closed
	if !alreadyClosed {
		s.closed = true
		close(s.closeDone)
	}
	waiter := s.waiter
	s.waiter = nil
	program, running := s.program, s.started
	s.mu.Unlock()
	if waiter != nil {
		waiter.result <- promptOutcome{err: context.Canceled}
	}
	if !alreadyClosed && program != nil {
		program.Quit()
	}
	if running {
		<-s.runDone
	}
	s.mu.Lock()
	err, canceled := s.runErr, s.canceled
	s.mu.Unlock()
	if canceled {
		return context.Canceled
	}
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

type promptFormMsg struct {
	id   uint64
	form *huh.Form
}

type promptSessionReadyMsg struct{}

type promptSessionModel struct {
	form    *huh.Form
	id      uint64
	ready   chan struct{}
	started bool
	finish  func(uint64, promptOutcome)
	cancel  func()
	queued  []tea.Msg
	size    *tea.WindowSizeMsg
	bg      *tea.BackgroundColorMsg
}

func (m *promptSessionModel) Init() tea.Cmd {
	return func() tea.Msg { return promptSessionReadyMsg{} }
}

func (m *promptSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
		m.cancel()
		return m, tea.Quit
	}
	switch msg := msg.(type) {
	case promptSessionReadyMsg:
		if !m.started {
			m.started = true
			close(m.ready)
		}
		return m, nil
	case promptFormMsg:
		if msg.id <= m.id {
			return m, nil
		}
		m.id, m.form = msg.id, msg.form
		cmds := []tea.Cmd{m.form.Init()}
		if m.size != nil {
			_, cmd := m.form.Update(*m.size)
			cmds = append(cmds, cmd)
		}
		if m.bg != nil {
			_, cmd := m.form.Update(*m.bg)
			cmds = append(cmds, cmd)
		}
		for m.form != nil && len(m.queued) > 0 {
			input := m.queued[0]
			m.queued[0] = nil
			m.queued = m.queued[1:]
			cmds = append(cmds, m.updateForm(input))
		}
		return m, tea.Batch(cmds...)
	case tea.WindowSizeMsg:
		m.size = &msg
	case tea.BackgroundColorMsg:
		m.bg = &msg
	case tea.KeyMsg, tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
		if m.form == nil {
			if len(m.queued) >= maxPromptHandoffKeys {
				m.finish(m.id, promptOutcome{err: fmt.Errorf("提示输入交接队列已满")})
				m.cancel()
				return m, tea.Quit
			}
			m.queued = append(m.queued, msg)
			return m, nil
		}
	}
	if m.form == nil {
		return m, nil
	}
	return m, m.updateForm(msg)
}

func (m *promptSessionModel) updateForm(msg tea.Msg) tea.Cmd {
	field := m.form.GetFocusedField()
	filtering := promptFiltering(field)
	_, cmd := m.form.Update(msg)
	if key, ok := msg.(tea.KeyMsg); ok && (key.String() == "enter" || key.String() == "tab") {
		if field.Error() == nil && !(filtering && key.String() == "enter") {
			// 私有提示始终只有一个 group/field。通过公开推进方法同步提交，
			// 不执行默认异步 NextField/NextGroup 链，避免后续键落到旧字段。
			m.form.NextField()
			m.form.NextGroup()
			cmd = nil
		}
	}
	if m.form.State == huh.StateCompleted {
		m.finish(m.id, promptOutcome{value: field.GetValue()})
		m.form = nil
		return nil
	}
	if m.form.State == huh.StateAborted {
		m.cancel()
		return tea.Quit
	}
	return cmd
}

func promptFiltering(field huh.Field) bool {
	switch field := field.(type) {
	case *huh.Select[string]:
		return field.GetFiltering()
	case *huh.Select[bool]:
		return field.GetFiltering()
	default:
		return false
	}
}

func (m *promptSessionModel) View() tea.View {
	if m.form == nil {
		return tea.NewView("")
	}
	view := tea.NewView(m.form.View())
	view.ReportFocus = true
	return view
}
