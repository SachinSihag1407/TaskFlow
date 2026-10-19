import { Compass } from "lucide-react";
import { usePageMeta } from "@/app/page-meta";
import { EmptyState } from "@/components/ui/feedback";
import { LinkButton } from "@/components/ui/button";

export default function NotFoundPage() {
  usePageMeta([{ label: "Not found" }]);
  return (
    <EmptyState
      icon={<Compass />}
      title="Page not found"
      description="The page you are looking for does not exist or was moved."
      action={
        <LinkButton to="/" variant="primary">
          Go to overview
        </LinkButton>
      }
    />
  );
}
