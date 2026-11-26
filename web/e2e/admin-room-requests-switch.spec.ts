import { test } from "@playwright/test";
import { adminRoomReadTests } from "./admin-room-reads";

test.use({ viewport: { width: 390, height: 844 } });

adminRoomReadTests({ path: "/admin/room-requests", otherRead: "/admin/rooms/locations", evidence: "admin-room-requests" });

adminRoomReadTests({ path: "/admin/rooms/locations", otherRead: "/admin/room-requests", evidence: "admin-room-locations" });
