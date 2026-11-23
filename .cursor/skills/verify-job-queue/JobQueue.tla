------------------------------ MODULE JobQueue ------------------------------
(***************************************************************************)
(* One row of the jobs table and the workers that claim it                 *)
(* (internal/jobs/jobs.go, internal/jobs/dead.go). Each action is one      *)
(* statement or one transaction in the code; SKILL.md maps every action to *)
(* its function. A handler run is not one step: it starts, then returns,   *)
(* waits (LaterIn), or its process stops.                                  *)
(*                                                                         *)
(* claim_seq is kept abstract. The code only asks whether a worker's       *)
(* number equals the row's, and a sequence never repeats a number, so the  *)
(* model keeps `holder`: the worker whose number the row carries, or       *)
(* NoOne once that worker has let go of it (finished or stopped).          *)
(*                                                                         *)
(* Break names one deliberate defect, each the removal of one guard in the *)
(* Go code; "none" is the code as it is. The check script runs each one    *)
(* and expects the rule it guards to fail.                                 *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    Workers,      \* worker goroutines, in one or several processes
    NoOne,        \* a model value: no running worker holds the row's claim_seq
    MaxAttempts,  \* the row's max_attempts
    MaxRetries,   \* how many times an admin presses Retry on the dead job
    MaxCrashes,   \* how many times a worker's process stops mid-claim
    MaxSlow,      \* how many times a lease runs out under a worker still running
    Break

Breaks == {"none", "no_claim_seq", "no_stopped_check", "on_dead_unchecked",
           "no_lease_recovery", "retry_unchecked", "no_failed_claim"}

ASSUME Break \in Breaks
ASSUME MaxAttempts \in 1..100   \* jobs_max_attempts_check
ASSUME NoOne \notin Workers

VARIABLES
    \* the row (holder stands for claim_seq)
    status, attempts, due, leaseExpired, holder,
    \* the world (known[w]: w's build has a handler for this kind; builds can
    \* differ while an upgrade is rolled back on some processes)
    known, graceOver, crashes, slow, retries,
    \* each worker's claim, as RunOnce holds it in `claimed`
    pc, myAttempts, myRound, outcome, lost,
    \* bookkeeping for the rules (not in the code)
    spent, hookCredit

rowVars   == <<status, attempts, due, leaseExpired>>
envVars   == <<known, graceOver, crashes, slow, retries>>
claimVars == <<holder, pc, myAttempts, myRound, outcome, lost>>
ruleVars  == <<spent, hookCredit>>
vars      == <<rowVars, envVars, claimVars, ruleVars>>

Statuses == {"queued", "running", "done", "failed", "dead", "dismissed", "gone"}
InFlight == {"claimed", "handler", "requeued", "finishing", "beforeHook"}

WorkerSymmetry == Permutations(Workers)

(* heldBy: id = ? AND status = 'running' AND claim_seq = ? *)
Held(w) == /\ status = "running"
           /\ (Break = "no_claim_seq" \/ holder = w)

(* RunOnce's first case: the last attempt never finished. *)
StoppedMidJob(w) == myAttempts[w] > MaxAttempts /\ Break # "no_stopped_check"

(* The worker's claim belongs to the current Retry round. *)
ThisRound(w) == myRound[w] = retries

(* One heldBy-guarded UPDATE: it lands only while w holds the job. *)
Land(w, st, dAttempts) ==
    IF Held(w)
    THEN /\ status' = st
         /\ attempts' = attempts + dAttempts
         /\ due' = FALSE
         /\ leaseExpired' = FALSE
    ELSE UNCHANGED rowVars

(* Once the claim is released, only a pending callback can remain. *)
LeaveClaim(w, nextPC) ==
    /\ holder' = IF holder = w THEN NoOne ELSE holder
    /\ pc' = [pc EXCEPT ![w] = nextPC]
    /\ myAttempts' = [myAttempts EXCEPT ![w] = 0]
    /\ myRound' = [myRound EXCEPT ![w] = 0]
    /\ outcome' = [outcome EXCEPT ![w] = "none"]
    /\ lost' = [lost EXCEPT ![w] = FALSE]

Release(w) == LeaveClaim(w, "idle")

(* A committed death earns this worker one hook call. Retry can start a *)
(* new round while the worker is still between the write and the call. *)
Dies(w) ==
    /\ Land(w, "dead", 0)
    /\ hookCredit' = [hookCredit EXCEPT ![w] = @ + (IF Held(w) THEN 1 ELSE 0)]
    /\ IF Held(w) \/ Break = "on_dead_unchecked"
       THEN LeaveClaim(w, "beforeHook")
       ELSE Release(w)

CallOnDead(w) ==
    /\ pc[w] = "beforeHook"
    /\ hookCredit' = [hookCredit EXCEPT ![w] = @ - 1]
    /\ Release(w)
    /\ UNCHANGED <<rowVars, envVars, spent>>

-----------------------------------------------------------------------------
(* Workers *)

(* RunOnce's claim UPDATE: a due queued or failed job, or a running one *)
(* whose lease ran out. One statement; SKIP LOCKED.                     *)
Claim(w) ==
    /\ pc[w] = "idle"
    /\ \/ status \in (IF Break = "no_failed_claim" THEN {"queued"} ELSE {"queued", "failed"})
          /\ due
       \/ status = "running" /\ leaseExpired /\ Break # "no_lease_recovery"
    /\ status' = "running"
    /\ attempts' = attempts + 1
    /\ due' = FALSE
    /\ leaseExpired' = FALSE
    /\ holder' = w
    /\ pc' = [pc EXCEPT ![w] = "claimed"]
    /\ myAttempts' = [myAttempts EXCEPT ![w] = attempts + 1]
    /\ myRound' = [myRound EXCEPT ![w] = retries]
    \* a lease taken back: whoever held the old claim has lost it
    /\ lost' = [v \in Workers |->
                   IF status = "running" /\ holder = v THEN TRUE ELSE lost[v]]
    /\ UNCHANGED <<envVars, outcome, ruleVars>>

(* RunOnce: stopped mid-job, or an unknown kind past its week of grace. *)
DieOnClaim(w) ==
    /\ pc[w] = "claimed"
    /\ StoppedMidJob(w) \/ (~known[w] /\ graceOver)
    /\ Dies(w)
    /\ UNCHANGED <<envVars, spent>>

(* RunOnce: an unknown kind waits an hour for a build that knows it and *)
(* gives its attempt back.                                              *)
WaitForABuild(w) ==
    /\ pc[w] = "claimed"
    /\ ~StoppedMidJob(w) /\ ~known[w] /\ ~graceOver
    /\ Land(w, "queued", -1)
    /\ Release(w)
    /\ UNCHANGED <<envVars, ruleVars>>

(* RunOnce: safeRun starts the handler. *)
StartHandler(w) ==
    /\ pc[w] = "claimed"
    /\ ~StoppedMidJob(w) /\ known[w]
    /\ pc' = [pc EXCEPT ![w] = "handler"]
    /\ spent' = spent + (IF ThisRound(w) THEN 1 ELSE 0)
    /\ UNCHANGED <<rowVars, envVars, holder, myAttempts, myRound, outcome, lost,
                   hookCredit>>

(* The handler returns nil, an error, or Later(at). A wait is not an    *)
(* attempt the job spent, whether or not its write lands.               *)
HandlerReturns(w) ==
    /\ pc[w] = "handler"
    /\ \E r \in {"ok", "err", "later"} :
         /\ outcome' = [outcome EXCEPT ![w] = r]
         /\ spent' = spent - (IF r = "later" /\ ThisRound(w) THEN 1 ELSE 0)
    /\ pc' = [pc EXCEPT ![w] = "finishing"]
    /\ UNCHANGED <<rowVars, envVars, holder, myAttempts, myRound, lost,
                   hookCredit>>

(* The handler's transaction calls LaterIn and commits with it; when the *)
(* claim is lost, LaterIn returns ErrClaimLost and the transaction rolls *)
(* back, and the handler returns that error.                             *)
HandlerWaitsIn(w) ==
    /\ pc[w] = "handler"
    /\ spent' = spent - (IF ThisRound(w) THEN 1 ELSE 0)
    /\ IF Held(w)
       THEN /\ Land(w, "queued", -1)
            /\ pc' = [pc EXCEPT ![w] = "requeued"]
            /\ UNCHANGED outcome
       ELSE /\ UNCHANGED rowVars
            /\ pc' = [pc EXCEPT ![w] = "finishing"]
            /\ outcome' = [outcome EXCEPT ![w] = "claimLost"]
    /\ UNCHANGED <<envVars, holder, myAttempts, myRound, lost, hookCredit>>

(* The handler returns Requeued(); finish writes nothing. *)
ReturnRequeued(w) ==
    /\ pc[w] = "requeued"
    /\ Release(w)
    /\ UNCHANGED <<rowVars, envVars, ruleVars>>

FinishWith(w, st, dAttempts) ==
    /\ Land(w, st, dAttempts)
    /\ Release(w)
    /\ UNCHANGED hookCredit

Finish(w) ==
    /\ pc[w] = "finishing"
    /\ CASE outcome[w] = "claimLost" -> Release(w) /\ UNCHANGED <<rowVars, hookCredit>>
         [] outcome[w] = "later" -> FinishWith(w, "queued", -1)
         [] outcome[w] = "ok" -> FinishWith(w, "done", 0)
         [] outcome[w] = "err" -> IF myAttempts[w] >= MaxAttempts
                                   THEN Dies(w)
                                   ELSE FinishWith(w, "failed", 0)
    /\ UNCHANGED <<envVars, spent>>

(* A crash after the dead write loses the pending callback. *)
Crash(w) ==
    /\ pc[w] \in InFlight
    /\ crashes < MaxCrashes
    /\ crashes' = crashes + 1
    /\ Release(w)
    /\ hookCredit' = [hookCredit EXCEPT ![w] = 0]
    /\ UNCHANGED <<rowVars, known, graceOver, slow, retries, spent>>

WorkerStep(w) ==
    \/ Claim(w) \/ DieOnClaim(w) \/ WaitForABuild(w) \/ StartHandler(w)
    \/ HandlerReturns(w) \/ HandlerWaitsIn(w) \/ ReturnRequeued(w) \/ Finish(w) \/ CallOnDead(w)

-----------------------------------------------------------------------------
(* Time and admins *)

(* locked_at falls behind now() - Lease. A stopped worker's lease always  *)
(* runs out; a slow worker's only MaxSlow times, which keeps attempts    *)
(* finite (two stalled claimers could otherwise trade the job forever).  *)
LeaseRunsOut ==
    /\ status = "running" /\ ~leaseExpired
    /\ holder = NoOne \/ slow < MaxSlow
    /\ leaseExpired' = TRUE
    /\ slow' = slow + (IF holder = NoOne THEN 0 ELSE 1)
    /\ UNCHANGED <<status, attempts, due, known, graceOver, crashes, retries,
                   claimVars, ruleVars>>

(* run_at arrives. *)
RunAtArrives ==
    /\ status \in {"queued", "failed"} /\ ~due
    /\ due' = TRUE
    /\ UNCHANGED <<status, attempts, leaseExpired, envVars, claimVars, ruleVars>>

(* created_at falls a week behind for a kind this build doesn't know. *)
GraceEnds ==
    /\ \E w \in Workers : ~known[w]
    /\ ~graceOver
    /\ graceOver' = TRUE
    /\ UNCHANGED <<rowVars, known, crashes, slow, retries, claimVars, ruleVars>>

(* jobs.Retry via decideDead: only a dead job, from its first attempt.  *)
AdminRetries ==
    /\ status = "dead" \/ (Break = "retry_unchecked" /\ status \in {"running", "done"})
    /\ retries < MaxRetries
    /\ status' = "queued"
    /\ attempts' = 0
    /\ due' = TRUE
    /\ leaseExpired' = FALSE
    /\ retries' = retries + 1
    /\ spent' = 0
    /\ UNCHANGED <<known, graceOver, crashes, slow, claimVars, hookCredit>>

(* jobs.Dismiss via decideDead. *)
AdminDismisses ==
    /\ status = "dead"
    /\ status' = "dismissed"
    /\ UNCHANGED <<attempts, due, leaseExpired, envVars, claimVars, ruleVars>>

(* PruneDone deletes a done or dismissed row. *)
PruneDone ==
    /\ status \in {"done", "dismissed"}
    /\ status' = "gone"
    /\ UNCHANGED <<attempts, due, leaseExpired, envVars, claimVars, ruleVars>>

(* Nothing left to happen: the row is deleted and every worker is idle. *)
Settled ==
    /\ status = "gone" /\ \A w \in Workers : pc[w] = "idle"
    /\ UNCHANGED vars

-----------------------------------------------------------------------------

Init ==
    /\ status = "queued" /\ attempts = 0
    /\ due = FALSE /\ leaseExpired = FALSE
    /\ known \in [Workers -> BOOLEAN] /\ graceOver = FALSE
    /\ crashes = 0 /\ slow = 0 /\ retries = 0
    /\ holder = NoOne
    /\ pc = [w \in Workers |-> "idle"]
    /\ myAttempts = [w \in Workers |-> 0]
    /\ myRound = [w \in Workers |-> 0]
    /\ outcome = [w \in Workers |-> "none"]
    /\ lost = [w \in Workers |-> FALSE]
    /\ spent = 0 /\ hookCredit = [w \in Workers |-> 0]

Next ==
    \/ \E w \in Workers : WorkerStep(w)
    \/ \E w \in Workers : Crash(w)
    \/ LeaseRunsOut \/ RunAtArrives \/ GraceEnds
    \/ AdminRetries \/ AdminDismisses \/ PruneDone
    \/ Settled

(* Every handler eventually returns or its process stops; workers keep  *)
(* polling; time passes. Admins and crashes get no fairness.            *)
Fairness ==
    /\ \A w \in Workers : WF_vars(WorkerStep(w))
    /\ WF_vars(LeaseRunsOut)
    /\ WF_vars(RunAtArrives)

Spec == Init /\ [][Next]_vars /\ Fairness

-----------------------------------------------------------------------------
(* Rules *)

TypeOK ==
    /\ status \in Statuses
    /\ attempts \in Int
    /\ holder \in Workers \cup {NoOne}
    /\ pc \in [Workers -> {"idle"} \cup InFlight]
    /\ outcome \in [Workers -> {"none", "ok", "err", "later", "claimLost"}]
    /\ lost \in [Workers -> BOOLEAN]

(* jobs_attempts_check: a give-back never takes attempts below zero. *)
AttemptsNeverNegative == attempts >= 0

(* A job's handler runs in two places at once only when the earlier     *)
(* run's lease ran out and another worker took the job back.            *)
TwoRunsOnlyAfterALeaseRunsOut ==
    Cardinality({w \in Workers : pc[w] \in {"claimed", "handler", "finishing"}
                                   /\ ~lost[w]}) <= 1

(* Charge starts and refund waits in the round that issued the claim. *)
AttemptBudget == spent <= MaxAttempts

(* A callback consumes the credit from its worker's committed death. *)
OnDeadAtMostOncePerDeath == \A w \in Workers : hookCredit[w] \in 0..1

(* A done or dismissed row stays terminal, including after pruning. *)
Finished == {"done", "dismissed", "gone"}
StaysFinished == status \in Finished => status' \in Finished
FinishedStaysFinished == [][StaysFinished]_vars

(* No job stays running forever: a lost worker's lease runs out. *)
RunningEnds == (status = "running") ~> (status # "running")

(* A job waiting to run is picked up. *)
WaitingIsPickedUp == (status \in {"queued", "failed"}) ~> (status \notin {"queued", "failed"})

=============================================================================
