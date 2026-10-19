import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Ban, MoreHorizontal, Pause, Pencil, Play, RotateCcw, Zap } from "lucide-react";
import type { ActionState, JobAction, JobStatus } from "@/lib/types";
import { useJobAction } from "@/api/hooks";
import { useCan, VIEWER_REASON } from "@/app/session";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import {
  Dropdown,
  DropdownContent,
  DropdownItem,
  DropdownSeparator,
  DropdownTrigger,
} from "@/components/ui/dropdown";

interface Props {
  jobId: string;
  jobName: string;
  status: JobStatus;
  actions: Record<JobAction, ActionState>;
  activeRuns: number;
}

/**
 * Header actions for a job. Availability comes from the server's state
 * machine (`actions`), and every unavailable action explains why.
 */
export function JobActions({ jobId, jobName, status, actions, activeRuns }: Props) {
  const navigate = useNavigate();
  const can = useCan();
  const mut = useJobAction();
  const [confirmCancel, setConfirmCancel] = useState(false);
  const writer = can("write");
  const reason = (a: JobAction) =>
    !writer ? VIEWER_REASON : actions[a].allowed ? undefined : actions[a].reason;
  const run = (action: "pause" | "resume" | "cancel" | "retry" | "run") =>
    mut.mutateAsync({ id: jobId, action });
  const busy = (a: string) => mut.isPending && mut.variables?.action === a;

  const primary =
    status === "PAUSED" ? (
      <Button
        variant="primary"
        icon={<Play className="h-3.5 w-3.5" />}
        disabledReason={reason("resume")}
        loading={busy("resume")}
        onClick={() => run("resume")}
      >
        Resume
      </Button>
    ) : actions.retry.allowed ? (
      <Button
        variant="primary"
        icon={<RotateCcw className="h-3.5 w-3.5" />}
        disabledReason={reason("retry")}
        loading={busy("retry")}
        onClick={() => run("retry")}
      >
        Retry
      </Button>
    ) : actions.run_now.allowed ? (
      <Button
        icon={<Zap className="h-3.5 w-3.5" />}
        disabledReason={reason("run_now")}
        loading={busy("run")}
        onClick={() => run("run")}
      >
        Run now
      </Button>
    ) : null;

  return (
    <>
      {primary}
      <Button
        icon={<Pencil className="h-3.5 w-3.5" />}
        disabledReason={reason("edit")}
        onClick={() => navigate(`/jobs/${jobId}/edit`)}
      >
        Edit
      </Button>
      <Dropdown>
        <DropdownTrigger asChild>
          <Button size="icon" aria-label="More actions">
            <MoreHorizontal className="h-4 w-4" />
          </Button>
        </DropdownTrigger>
        <DropdownContent className="w-64">
          <DropdownItem
            icon={<Zap />}
            disabled={!!reason("run_now")}
            hint={reason("run_now")}
            onSelect={() => run("run")}
          >
            Run now
          </DropdownItem>
          <DropdownItem
            icon={<Pause />}
            disabled={!!reason("pause")}
            hint={reason("pause")}
            onSelect={() => run("pause")}
          >
            Pause
          </DropdownItem>
          <DropdownItem
            icon={<Play />}
            disabled={!!reason("resume")}
            hint={reason("resume")}
            onSelect={() => run("resume")}
          >
            Resume
          </DropdownItem>
          <DropdownItem
            icon={<RotateCcw />}
            disabled={!!reason("retry")}
            hint={reason("retry")}
            onSelect={() => run("retry")}
          >
            Retry failed run
          </DropdownItem>
          <DropdownSeparator />
          <DropdownItem
            icon={<Ban />}
            danger
            disabled={!!reason("cancel")}
            hint={reason("cancel")}
            onSelect={() => setConfirmCancel(true)}
          >
            Cancel job…
          </DropdownItem>
        </DropdownContent>
      </Dropdown>
      <ConfirmDialog
        open={confirmCancel}
        onOpenChange={setConfirmCancel}
        title={`Cancel “${jobName}”?`}
        tone="danger"
        confirmLabel="Cancel job"
        onConfirm={() => run("cancel")}
        description={
          <>
            No further runs will be scheduled.{" "}
            {activeRuns > 0
              ? `${activeRuns} active run(s) will be cancelled; runs already executing are asked to stop and may still finish.`
              : "There are no active runs."}{" "}
            This cannot be undone.
          </>
        }
      />
    </>
  );
}
