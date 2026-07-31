import release from "../release-data.json";

export type TourListing = {
  id: string;
  title: string;
  description: string;
  url: string;
  version?: string;
  commit?: string;
  proofs?: string[];
  released: boolean;
};

const previewTours: TourListing[] = [
  {
    id: "context-switching",
    title: "Move between host and VM contexts",
    description:
      "Start Alpine, run commands in its persistent shell, return to the host, and resume the same VM context.",
    url: "/tours/context-switching.cast",
    version: "development",
    proofs: [
      "vmsh waits for a selected VM to become ready before returning control.",
      "Ordinary commands run in the selected system without repeating a target.",
      "Returning to the host does not discard the VM's persistent shell state.",
    ],
    released: false,
  },
];

const releasedTours = (release.tours as Array<{
  id: string;
  title: string;
  description?: string;
  url: string;
  version?: string;
  commit?: string;
}>).map((tour): TourListing => ({
  ...tour,
  description: tour.description ?? "A tested guided vmsh terminal session.",
  released: true,
}));

export const tours = [...releasedTours, ...previewTours.filter(
  (preview) => !releasedTours.some((released) => released.id === preview.id),
)].sort((a, b) => a.id.localeCompare(b.id));

export function findTour(id: string) {
  return tours.find((tour) => tour.id === id);
}

export function tourAssetUrl(tour: TourListing) {
  const basePath = process.env.NEXT_PUBLIC_VMSH_BASE_PATH ?? "";
  return `${basePath}${tour.url}`;
}

export const releaseTag = release.tag;
