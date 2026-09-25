import os
import sys
import threading
import time

import pytest

from agentpulse import Activity, Config, Recorder, User, scope

from fakes import MemorySink


def recorder(sink=None, **config):
    config.setdefault("exit_timeout", 0)
    return Recorder(Config(sinks=[sink] if sink else [], **config))


def test_a_full_queue_drops_and_counts_rather_than_waiting():
    stuck = threading.Event()

    class Stuck(MemorySink):
        def send(self, activities, workspace, timeout):
            stuck.wait(5)

    rec = recorder(Stuck(), queue_size=10, batch_size=1)
    started = time.monotonic()
    for _ in range(1000):
        rec.record(Activity(request="r"))
    assert time.monotonic() - started < 0.5
    stats = rec.stats()
    assert stats.recorded + stats.dropped == 1000
    assert stats.dropped >= 1000 - 11
    stuck.set()


def test_flush_delivers_what_is_queued():
    sink = MemorySink()
    rec = recorder(sink, flush_every=60)
    for i in range(3):
        rec.record(Activity(request=f"r{i}"))
    assert rec.flush(timeout=2)
    assert [a.request for a in sink.activities] == ["r0", "r1", "r2"]
    assert rec.stats().delivered == 3


def test_a_quiet_agent_still_reports():
    sink = MemorySink()
    rec = recorder(sink, flush_every=0.05)
    rec.record(Activity(request="r"))
    deadline = time.monotonic() + 2
    while not sink.activities and time.monotonic() < deadline:
        time.sleep(0.01)
    assert sink.activities


def test_a_full_batch_goes_out_without_waiting_for_the_timer():
    sink = MemorySink()
    rec = recorder(sink, batch_size=5, flush_every=60)
    for _ in range(5):
        rec.record(Activity(request="r"))
    deadline = time.monotonic() + 2
    while len(sink.activities) < 5 and time.monotonic() < deadline:
        time.sleep(0.01)
    assert len(sink.batches[0][1]) == 5


def test_a_failing_sink_is_counted_and_does_not_affect_another():
    good, bad = MemorySink(), MemorySink(fail=True)
    rec = Recorder(Config(sinks=[good, bad], exit_timeout=0))
    rec.record(Activity(request="r"))
    rec.flush(timeout=2)
    assert len(good.activities) == 1
    stats = rec.stats()
    assert stats.delivered == 1 and stats.failed == 1


def test_a_slow_sink_does_not_delay_another():
    fast, slow = MemorySink(), MemorySink(delay=1.0)
    rec = Recorder(Config(sinks=[slow, fast], exit_timeout=0))
    rec.record(Activity(request="r"))
    rec.flush(timeout=0)
    deadline = time.monotonic() + 0.5
    while not fast.activities and time.monotonic() < deadline:
        time.sleep(0.01)
    assert fast.activities and not slow.activities


def test_flush_never_waits_longer_than_it_was_told():
    rec = recorder(MemorySink(delay=2.0))
    rec.record(Activity(request="r"))
    started = time.monotonic()
    assert rec.flush(timeout=0.2) is False
    assert time.monotonic() - started < 0.5


def test_close_drains_and_later_records_are_dropped_and_counted():
    sink = MemorySink()
    rec = recorder(sink, flush_every=60)
    rec.record(Activity(request="before"))
    assert rec.close(timeout=2)
    rec.record(Activity(request="after"))
    assert [a.request for a in sink.activities] == ["before"]
    assert rec.stats().dropped == 1


def test_a_recorder_that_never_records_starts_nothing():
    before = threading.active_count()
    rec = recorder(MemorySink())
    assert threading.active_count() == before
    assert rec.flush(timeout=0.1) and rec.close(timeout=0.1)


def test_one_flush_is_one_batch_per_workspace_and_one_tenant_failing_leaves_the_other():
    class Picky(MemorySink):
        def send(self, activities, workspace, timeout):
            if workspace == "bad":
                raise RuntimeError("refused")
            super().send(activities, workspace, timeout)

    sink = Picky()
    rec = recorder(sink, flush_every=60)
    with scope(workspace="acme"):
        rec.record(Activity(request="a1"))
    with scope(workspace="bad"):
        rec.record(Activity(request="b1"))
    rec.record(Activity(request="none"))
    with scope(workspace="acme"):
        rec.record(Activity(request="a2"))
    rec.flush(timeout=2)
    assert {w: [a.request for a in batch] for w, batch in sink.batches} == {"acme": ["a1", "a2"], "": ["none"]}
    assert rec.stats().failed == 1 and rec.stats().delivered == 3


def test_a_person_is_named_once_and_again_when_their_name_changes():
    sink = MemorySink()
    rec = recorder(sink, flush_every=60)
    for _ in range(3):
        rec.note_user(User("u1", "Ada"))
    rec.note_user(User("u1", "Ada Lovelace"))
    rec.note_user(User("u2"))
    rec.flush(timeout=2)
    assert [u.name for _, users in sink.named for u in users] == ["Ada", "Ada Lovelace"]
    assert rec.stats().named == 2


def test_an_identifier_with_a_slash_cannot_be_named_and_is_counted():
    rec = recorder(MemorySink())
    rec.note_user(User("users/u1", "Ada"))
    assert rec.stats().names_failed == 1


def test_a_person_a_sink_refused_is_named_again_next_time():
    sink = MemorySink(fail=True)
    rec = recorder(sink, flush_every=60)
    rec.note_user(User("u1", "Ada"))
    rec.flush(timeout=2)
    sink.fail = False
    rec.note_user(User("u1", "Ada"))
    rec.flush(timeout=2)
    assert rec.stats().names_failed == 1
    assert [u.id for _, users in sink.named for u in users] == ["u1"]


def test_a_person_is_named_in_each_workspace_they_act_in():
    sink = MemorySink()
    rec = recorder(sink, flush_every=60)
    with scope(workspace="w1"):
        rec.note_user(User("u1", "Ada"))
    with scope(workspace="w2"):
        rec.note_user(User("u1", "Ada"))
    rec.flush(timeout=2)
    assert sorted(w for w, _ in sink.named) == ["w1", "w2"]


def test_a_sink_that_takes_no_names_is_given_none():
    class RecordsOnly:
        name = "records-only"

        def __init__(self):
            self.got = []

        def send(self, activities, workspace, timeout):
            self.got.extend(activities)

    sink = RecordsOnly()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0))
    rec.note_user(User("u1", "Ada"))
    rec.record(Activity(request="r"))
    rec.flush(timeout=2)
    assert len(sink.got) == 1 and rec.stats().names_failed == 0


@pytest.mark.skipif(not hasattr(os, "fork"), reason="no fork on this platform")
@pytest.mark.filterwarnings("ignore:This process .* is multi-threaded:DeprecationWarning")
def test_a_forked_child_records_with_its_own_worker_and_not_the_parent_queue():
    read_end, write_end = os.pipe()

    class Pipe(MemorySink):
        def send(self, activities, workspace, timeout):
            os.write(write_end, ",".join(a.request for a in activities).encode() + b"\n")

    rec = Recorder(Config(sinks=[Pipe()], exit_timeout=0, flush_every=60))
    rec.record(Activity(request="parent"))

    pid = os.fork()
    if pid == 0:
        try:
            rec.record(Activity(request="child"))
            ok = rec.flush(timeout=2)
            os._exit(0 if ok else 1)
        except BaseException:
            os._exit(2)
    _, status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(status) == 0
    rec.flush(timeout=2)
    os.close(write_end)
    lines = sorted(os.read(read_end, 4096).decode().split())
    os.close(read_end)
    assert lines == ["child", "parent"]


def test_recording_from_many_threads_loses_nothing_the_queue_had_room_for():
    sink = MemorySink()
    rec = recorder(sink, queue_size=100_000, flush_every=60)

    def burst():
        for _ in range(1000):
            rec.record(Activity(request="r"))

    threads = [threading.Thread(target=burst) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    rec.flush(timeout=5)
    assert len(sink.activities) == 8000 == rec.stats().delivered


@pytest.mark.skipif(sys.platform == "win32", reason="uses a subprocess with a POSIX shell exit")
def test_what_is_queued_at_exit_is_sent_within_the_exit_limit(tmp_path):
    import subprocess

    out = tmp_path / "sent.txt"
    script = f"""
import sys
sys.path.insert(0, {str(os.path.dirname(__file__) + '/../src')!r})
from agentpulse import Activity, Config, Recorder
class File:
    name = "file"
    def send(self, activities, workspace, timeout):
        open({str(out)!r}, "a").write("".join(a.request + "\\n" for a in activities))
rec = Recorder(Config(sinks=[File()], flush_every=60, exit_timeout=2))
rec.record(Activity(request="last"))
"""
    subprocess.run([sys.executable, "-c", script], check=True, timeout=10)
    assert out.read_text().split() == ["last"]
