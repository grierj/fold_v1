-- +goose Up
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION public.reject_terminal_contact_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION '% is immutable', TG_ARGV[0]
        USING ERRCODE = '23514', CONSTRAINT = TG_NAME;
END;
$$;
-- +goose StatementEnd

-- Reason text has its own erasure lifecycle (DEC-141). The row key remains
-- governed by its primary key and the grant's foreign key.
CREATE TRIGGER contact_request_terminal BEFORE UPDATE ON public.contact_requests FOR EACH ROW
    WHEN (OLD.purpose = 'contact' AND OLD.status <> 'pending'
        AND ROW(NEW.requester_id, NEW.subject_user_id, NEW.context_type, NEW.context_id, NEW.fields,
                NEW.needs_admin, NEW.created_at, NEW.expires_at, NEW.status, NEW.completed_at,
                NEW.subject_decision, NEW.subject_decided_by, NEW.subject_decided_at,
                NEW.admin_decision, NEW.admin_decided_by, NEW.admin_decided_at, NEW.withdrawn_by_id, NEW.end_reason)
            IS DISTINCT FROM
            ROW(OLD.requester_id, OLD.subject_user_id, OLD.context_type, OLD.context_id, OLD.fields,
                OLD.needs_admin, OLD.created_at, OLD.expires_at, OLD.status, OLD.completed_at,
                OLD.subject_decision, OLD.subject_decided_by, OLD.subject_decided_at,
                OLD.admin_decision, OLD.admin_decided_by, OLD.admin_decided_at, OLD.withdrawn_by_id, OLD.end_reason))
    EXECUTE FUNCTION public.reject_terminal_contact_update('a finished contact request outcome');

CREATE TRIGGER contact_grant_terminal BEFORE UPDATE ON public.contact_grants FOR EACH ROW
    WHEN (OLD.ended_at IS NOT NULL AND NEW IS DISTINCT FROM OLD)
    EXECUTE FUNCTION public.reject_terminal_contact_update('an ended contact grant');

-- +goose Down
SET LOCAL lock_timeout = '5s';

DROP TRIGGER contact_grant_terminal ON public.contact_grants;
DROP TRIGGER contact_request_terminal ON public.contact_requests;
DROP FUNCTION public.reject_terminal_contact_update();
